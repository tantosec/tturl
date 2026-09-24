package timing

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http/httpguts"
)

const h1RecordLimit = 16384

// h1Prepared owns immutable, validated request records for one sender.
type h1Prepared struct {
	request *http.Request
	records [][]byte
	final   []byte
}

type h1Observation struct {
	initial, final, written, firstHeaders, finalHeaders, complete time.Time
	status                                                        int
	header                                                        http.Header
	trailer                                                       http.Header
	bodyBytes                                                     int64
	receivedBytes                                                 int64
	declaredLength                                                int64
	lengthPresent                                                 bool
	retired, eofObserved                                          bool
	err                                                           error
	responseErr                                                   error
}

// prepareH1 validates explicit framing before net/http serialises any bytes.
func prepareH1(request *http.Request, body []byte, single, last bool) (*h1Prepared, error) {
	if request == nil || request.URL == nil {
		return nil, errors.New("missing request URL")
	}
	if request.Method != "" && !httpguts.ValidHeaderFieldName(request.Method) {
		return nil, errors.New("invalid request method")
	}
	authority := request.Host
	if authority == "" {
		authority = request.URL.Host
	}
	if authority == "" || !httpguts.ValidHostHeader(authority) {
		return nil, errors.New("invalid request authority")
	}
	header := make(http.Header, len(request.Header))
	for key, values := range request.Header {
		if !httpguts.ValidHeaderFieldName(key) {
			return nil, errors.New("invalid request header name")
		}
		canonical := http.CanonicalHeaderKey(key)
		header[canonical] = append(header[canonical], values...)
	}
	request = request.Clone(context.Background())
	request.Header = header
	if request.Method == http.MethodConnect || request.Header.Get("Upgrade") != "" ||
		httpguts.HeaderValuesContainsToken(request.Header.Values("Connection"), "upgrade") {
		return nil, errors.New("upgrades and tunnels are unsupported")
	}
	for key, values := range request.Header {
		if !httpguts.ValidHeaderFieldName(key) {
			return nil, errors.New("invalid request header name")
		}
		for _, value := range values {
			if !httpguts.ValidHeaderFieldValue(value) {
				return nil, errors.New("invalid request header value")
			}
		}
	}
	length, present, err := h1Length(request.Header)
	if err != nil {
		return nil, err
	}
	coding := request.Header.Values("Transfer-Encoding")
	chunked := len(coding) != 0 || len(request.TransferEncoding) != 0
	if chunked && present {
		return nil, errors.New("Content-Length conflicts with Transfer-Encoding")
	}
	if len(coding) > 0 && (len(coding) != 1 || !strings.EqualFold(strings.Trim(coding[0], " \t"), "chunked")) {
		return nil, errors.New("unsupported transfer coding")
	}
	if len(request.TransferEncoding) > 0 &&
		(len(request.TransferEncoding) != 1 || request.TransferEncoding[0] != "chunked") {
		return nil, errors.New("unsupported transfer coding")
	}
	if present && length != int64(len(body)) {
		return nil, errors.New("request Content-Length mismatch")
	}
	if len(request.Trailer) > 0 && !chunked {
		return nil, errors.New("request trailers require chunked framing")
	}
	for key, values := range request.Trailer {
		if !httpguts.ValidTrailerHeader(key) || !httpguts.ValidHeaderFieldName(key) {
			return nil, errors.New("invalid request trailer")
		}
		for _, value := range values {
			if !httpguts.ValidHeaderFieldValue(value) {
				return nil, errors.New("invalid request trailer value")
			}
		}
	}
	clone := request
	clone.Header.Del("Transfer-Encoding")
	clone.ContentLength = int64(len(body))
	clone.Body = io.NopCloser(bytes.NewReader(body))
	if len(body) == 0 && !chunked {
		clone.Body = http.NoBody
	}
	if chunked {
		clone.TransferEncoding = []string{"chunked"}
	}
	var wire bytes.Buffer
	if err := clone.Write(&wire); err != nil {
		return nil, err
	}
	encoded := wire.Bytes()
	if present && !bytes.Contains(encoded[:bytes.Index(encoded, []byte("\r\n\r\n"))], []byte("\r\nContent-Length:")) {
		end := bytes.Index(encoded, []byte("\r\n\r\n"))
		field := fmt.Appendf(nil, "\r\nContent-Length: %d", length)
		preserved := append(bytes.Clone(encoded[:end]), field...)
		encoded = append(preserved, encoded[end:]...)
	}
	var final []byte
	if last {
		final = bytes.Clone(encoded[len(encoded)-1:])
		encoded = encoded[:len(encoded)-1]
	}
	if single && len(encoded) > h1RecordLimit {
		return nil, fmt.Errorf("request record exceeds %d bytes", h1RecordLimit)
	}
	var records [][]byte
	for len(encoded) > 0 {
		size := min(len(encoded), h1RecordLimit)
		records = append(records, bytes.Clone(encoded[:size]))
		encoded = encoded[size:]
	}
	return &h1Prepared{request: clone, records: records, final: final}, nil
}

func h1Length(header http.Header) (int64, bool, error) {
	values := header.Values("Content-Length")
	var length int64
	for index, value := range values {
		value = strings.Trim(value, " \t")
		if value == "" {
			return 0, true, errors.New("invalid Content-Length")
		}
		for _, char := range value {
			if char < '0' || char > '9' {
				return 0, true, errors.New("invalid Content-Length")
			}
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || index > 0 && parsed != length {
			return 0, true, errors.New("invalid or conflicting Content-Length")
		}
		length = parsed
	}
	return length, len(values) > 0, nil
}

// runH1 starts reading before release and disposes of interrupted exchanges.
// Gates and phase budgets are supplied by the campaign coordinator.
func runH1(ctx context.Context, conn net.Conn, prepared *h1Prepared,
	start, finish <-chan struct{}, headerMax, bodyMax int64,
) h1Observation {
	return runH1WithRelease(ctx, conn, prepared, start, finish, headerMax, bodyMax, nil)
}

func runH1WithRelease(ctx context.Context, conn net.Conn, prepared *h1Prepared,
	start, finish <-chan struct{}, headerMax, bodyMax int64, beforeInitial func(time.Time) error,
) h1Observation {
	return runH1WithHooks(ctx, conn, prepared, start, finish, headerMax, bodyMax, beforeInitial, nil)
}

func runH1WithHooks(ctx context.Context, conn net.Conn, prepared *h1Prepared,
	start, finish <-chan struct{}, headerMax, bodyMax int64,
	beforeInitial func(time.Time) error, prefixComplete func() error,
) h1Observation {
	return runH1WithObserver(ctx, conn, prepared, start, finish, headerMax, bodyMax,
		beforeInitial, prefixComplete, nil)
}

func runH1WithObserver(ctx context.Context, conn net.Conn, prepared *h1Prepared,
	start, finish <-chan struct{}, headerMax, bodyMax int64,
	beforeInitial func(time.Time) error, prefixComplete func() error, observed func(h1Observation),
) h1Observation {
	return runH1WithCallbacks(ctx, conn, prepared, start, finish, headerMax, bodyMax,
		beforeInitial, prefixComplete, observed, nil, nil)
}

func runH1WithCallbacks(ctx context.Context, conn net.Conn, prepared *h1Prepared,
	start, finish <-chan struct{}, headerMax, bodyMax int64,
	beforeInitial func(time.Time) error, prefixComplete func() error,
	observed func(h1Observation), consume func([]byte, h1Observation) error,
	exchangeComplete func(time.Time) error,
) h1Observation {
	var observation h1Observation
	if _, err := h1ReaderSize(headerMax); err != nil {
		observation.err = err
		return observation
	}
	var mutex sync.Mutex
	readerFinished, writerFinished, completionCalled := false, false, false
	completedLocked := func() {
		if readerFinished && writerFinished && !completionCalled {
			completionCalled = true
			if exchangeComplete != nil {
				if err := exchangeComplete(time.Now()); err != nil && observation.err == nil {
					observation.err = err
				}
			}
		}
	}
	received := make(chan struct{})
	failed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	go func() {
		defer close(received)
		result := readH1WithCallbacks(conn, prepared.request, headerMax, bodyMax, consume,
			func() {
				mutex.Lock()
				readerFinished = true
				completedLocked()
				mutex.Unlock()
			})
		if result.err != nil {
			close(failed)
			_ = conn.Close()
		}
		mutex.Lock()
		observation.firstHeaders = result.firstHeaders
		observation.finalHeaders = result.finalHeaders
		observation.complete = result.complete
		observation.status, observation.header = result.status, result.header
		observation.bodyBytes, observation.retired = result.bodyBytes, result.retired
		observation.receivedBytes = result.receivedBytes
		observation.declaredLength, observation.lengthPresent = result.declaredLength, result.lengthPresent
		observation.trailer = result.trailer
		observation.eofObserved = result.eofObserved
		observation.responseErr = result.err
		if observation.err == nil {
			observation.err = result.err
		}
		mutex.Unlock()
		if observed != nil {
			observed(result)
		}
	}()
	write := func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-failed:
			return errors.New("response failed before release")
		case <-start:
		}
		for index, record := range prepared.records {
			select {
			case <-failed:
				return errors.New("response failed before request write")
			default:
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			now := time.Now()
			if index == 0 && beforeInitial != nil {
				if err := beforeInitial(now); err != nil {
					return err
				}
			}
			mutex.Lock()
			if index == 0 {
				observation.initial = now
			}
			if index == len(prepared.records)-1 && len(prepared.final) == 0 {
				observation.final = now
			}
			mutex.Unlock()
			if count, err := conn.Write(record); err != nil {
				return err
			} else if count != len(record) {
				return io.ErrShortWrite
			}
		}
		if prefixComplete != nil {
			if err := prefixComplete(); err != nil {
				return err
			}
		}
		if len(prepared.final) > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-failed:
				return errors.New("response failed before finalisation")
			case <-finish:
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			select {
			case <-failed:
				return errors.New("response failed before finalisation")
			default:
			}
			mutex.Lock()
			observation.final = time.Now()
			mutex.Unlock()
			if count, err := conn.Write(prepared.final); err != nil {
				return err
			} else if count != len(prepared.final) {
				return io.ErrShortWrite
			}
		}
		mutex.Lock()
		observation.written = time.Now()
		mutex.Unlock()
		return nil
	}
	if err := write(); err != nil {
		mutex.Lock()
		observation.err = err
		mutex.Unlock()
		_ = conn.Close()
	}
	mutex.Lock()
	writerFinished = true
	completedLocked()
	mutex.Unlock()
	<-received
	if ctx.Err() != nil {
		observation.err = ctx.Err()
	}
	if observation.err != nil || observation.retired {
		_ = conn.Close()
	}
	return observation
}

func readH1(conn net.Conn, request *http.Request, headerMax, bodyMax int64) h1Observation {
	return readH1WithConsume(conn, request, headerMax, bodyMax, nil)
}

// readH1WithConsume observes EOF before handing returned bytes to a consumer.
// Byte slices belong to the reader and remain valid only during the callback.
func readH1WithConsume(conn net.Conn, request *http.Request, headerMax, bodyMax int64,
	consume func([]byte, h1Observation) error,
) h1Observation {
	return readH1WithCallbacks(conn, request, headerMax, bodyMax, consume, nil)
}

func readH1WithCallbacks(conn net.Conn, request *http.Request, headerMax, bodyMax int64,
	consume func([]byte, h1Observation) error, terminal func(),
) (result h1Observation) {
	terminalCalled := false
	finish := func() {
		if !terminalCalled {
			terminalCalled = true
			if terminal != nil {
				terminal()
			}
		}
	}
	defer finish()
	readerSize, err := h1ReaderSize(headerMax)
	if err != nil {
		result.err = err
		return result
	}
	wireReader := bufio.NewReaderSize(conn, readerSize)
	for {
		raw, first, err := h1ReadHeaders(wireReader, headerMax+4096)
		if result.firstHeaders.IsZero() {
			result.firstHeaders = first
		}
		if err != nil {
			result.err = err
			return result
		}
		headerReader := bufio.NewReader(bytes.NewReader(raw))
		statusLine, err := headerReader.ReadString('\n')
		if err != nil {
			result.err = err
			return result
		}
		fields, err := textproto.NewReader(headerReader).ReadMIMEHeader()
		if err != nil {
			result.err = err
			return result
		}
		headers := http.Header(fields)
		result.header = headers
		status, err := h1Status(statusLine)
		result.status = status
		if err != nil {
			result.err = err
			return result
		}
		if err := h1DecodedHeaders(headers, headerMax, false); err != nil {
			result.err = err
			return result
		}
		length, present, err := h1Length(headers)
		result.declaredLength, result.lengthPresent = length, present
		if err != nil {
			result.err = err
			return result
		}
		if present && len(headers.Values("Transfer-Encoding")) > 0 {
			result.err = errors.New("response Content-Length conflicts with Transfer-Encoding")
			return result
		}
		// Replay owns only this section; wireReader retains the connection.
		// Informational responses discard replay without retaining a chain.
		reader := bufio.NewReaderSize(io.MultiReader(bytes.NewReader(raw), wireReader), readerSize)
		response, err := http.ReadResponse(reader, request)
		if err != nil {
			result.err = err
			return result
		}
		if response.StatusCode >= 100 && response.StatusCode < 200 && response.StatusCode != http.StatusSwitchingProtocols {
			_ = response.Body.Close()
			continue
		}
		result.finalHeaders = time.Now()
		result.status, result.header = response.StatusCode, response.Header.Clone()
		if response.StatusCode == http.StatusSwitchingProtocols {
			result.err = errors.New("switching protocols is unsupported")
			result.retired = true
			return result
		}
		result.retired = response.Close
		closeDelimited := response.ContentLength < 0 && len(response.TransferEncoding) == 0 &&
			response.Body != http.NoBody
		buffer := make([]byte, 32*1024)
		for {
			destination := buffer
			if bodyMax >= 0 && bodyMax-result.bodyBytes < int64(len(destination)) {
				destination = destination[:int(bodyMax-result.bodyBytes)+1]
			}
			count, readErr := response.Body.Read(destination)
			result.receivedBytes += int64(count)
			if errors.Is(readErr, io.EOF) {
				result.complete = time.Now()
				result.eofObserved = closeDelimited
			}
			accepted := count
			if bodyMax >= 0 && int64(accepted) > bodyMax-result.bodyBytes {
				accepted = int(bodyMax - result.bodyBytes)
				result.err = errors.New("response body limit exceeded")
			}
			result.bodyBytes += int64(accepted)
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				result.err = readErr
			}
			if errors.Is(readErr, io.EOF) {
				if err := h1DecodedHeaders(response.Trailer, headerMax, true); err != nil && result.err == nil {
					result.err = err
				}
				if (reader.Buffered() > 0 || wireReader.Buffered() > 0) && result.err == nil {
					result.err = errors.New("unexpected bytes after response")
				}
			}
			if result.err != nil || errors.Is(readErr, io.EOF) {
				finish()
			}
			if errors.Is(readErr, io.EOF) {
				result.trailer = response.Trailer.Clone()
			}
			if consume != nil && (accepted > 0 || errors.Is(readErr, io.EOF)) {
				if err := consume(destination[:accepted], result); err != nil && result.err == nil {
					result.err = err
				}
			}
			if result.err != nil {
				return result
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
		}
		_ = response.Body.Close()
		return result
	}
}

func h1ReadHeaders(reader *bufio.Reader, maximum int64) ([]byte, time.Time, error) {
	var raw bytes.Buffer
	var first time.Time
	statusLine := true
	for {
		var line strings.Builder
		for {
			fragment, err := reader.ReadSlice('\n')
			if first.IsZero() && len(fragment) > 0 {
				first = time.Now()
			}
			if maximum >= 0 && int64(raw.Len()+line.Len()+len(fragment)) > maximum {
				return nil, first, errors.New("response header limit exceeded")
			}
			line.Write(fragment)
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil {
				return nil, first, err
			}
			break
		}
		if !strings.HasSuffix(line.String(), "\r\n") {
			return nil, first, errors.New("invalid response header line ending")
		}
		if !statusLine && line.String() != "\r\n" {
			key, value, found := strings.Cut(strings.TrimSuffix(line.String(), "\r\n"), ":")
			if !found || !httpguts.ValidHeaderFieldName(key) || !httpguts.ValidHeaderFieldValue(value) {
				return nil, first, errors.New("invalid response header field")
			}
		}
		statusLine = false
		raw.WriteString(line.String())
		if line.String() == "\r\n" {
			return raw.Bytes(), first, nil
		}
	}
}

// h1ReaderSize gives net/http's bounded trailer parser enough space for every
// permitted decoded section. The command always supplies a finite header bound.
func h1ReaderSize(maximum int64) (int, error) {
	if maximum < 0 || maximum > int64(math.MaxInt)-4096 {
		return 0, errors.New("HTTP/1.1 requires a finite supported response header bound")
	}
	return int(maximum) + 4096, nil
}

func h1DecodedHeaders(header http.Header, maximum int64, trailers bool) error {
	var size int64
	for key, values := range header {
		if !httpguts.ValidHeaderFieldName(key) || trailers && !httpguts.ValidTrailerHeader(key) {
			return errors.New("invalid response header field")
		}
		for _, value := range values {
			if !httpguts.ValidHeaderFieldValue(value) {
				return errors.New("invalid response header value")
			}
			cost := int64(len(key)) + int64(len(value)) + 32
			if cost > maximum-size {
				return errors.New("decoded response header limit exceeded")
			}
			size += cost
		}
	}
	return nil
}

// h1Status checks the wire status code before supplying a validated endpoint.
// net/http accepts signed three-character codes and codes outside 100..599.
func h1Status(line string) (int, error) {
	version, rest, found := strings.Cut(strings.TrimSuffix(line, "\r\n"), " ")
	if !found || version != "HTTP/1.1" && version != "HTTP/1.0" || len(rest) < 4 || rest[3] != ' ' {
		return 0, errors.New("invalid HTTP/1.1 response status syntax")
	}
	code := rest[:3]
	status, err := strconv.Atoi(code)
	if err != nil {
		return 0, errors.New("invalid HTTP/1.1 response status code")
	}
	for _, digit := range code {
		if digit < '0' || digit > '9' {
			return status, errors.New("invalid HTTP/1.1 response status code")
		}
	}
	if status < 100 || status > 599 || !httpguts.ValidHeaderFieldValue(rest[4:]) {
		return status, errors.New("invalid HTTP/1.1 response status code or reason")
	}
	return status, nil
}
