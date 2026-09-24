// Package tth2 releases HTTP/2 request batches and reports response arrival
// order for timeless timing attacks.
//
// [Client.SendBatch] sends a same-origin batch once and returns a
// [BatchResult]. Each [Result] records its [Result.ArrivalRank], and
// [BatchResult.ArrivalOrder] records the inverse mapping. These ordinal results
// avoid comparing absolute durations from separate network trips, although
// HTTP/2 parsing and dispatch, peer scheduling, queueing and contention, and
// intermediaries can still affect the observed order.
//
// # Shared-record batch completion
//
// A conforming TLS endpoint authenticates a complete application-data record
// before releasing its plaintext to HTTP/2. When every request completes with
// headers, tth2 places those finalising frames in one record. When every
// request has a non-empty body, their final DATA and END_STREAM frames share a
// later record. Each case has a shared completion gate despite TCP segmentation
// or retransmission. A batch mixing those completion points has no single gate.
// The shared gate is intended to encourage near-simultaneous arrival and may
// enable concurrent handling. HTTP/2 still parses and dispatches frames
// sequentially after that gate, while the network, peer, runtime, and
// application remain free to add skew.
//
// tth2 limits each load-bearing flush to the 16 KiB TLS plaintext maximum and
// rejects one that cannot fit. It also disables dynamic TLS record sizing on a
// clone of [Transport.TLSClientConfig], preventing an early connection from
// splitting a fitting flush across smaller records.
//
// Bodyless requests end every stream in one HEADERS flush. For requests with
// bodies, tth2 releases all headers together.
//
// With positive withholding, tth2 transfers body prefixes and releases each
// withheld tail with END_STREAM in one final record. Configure the tail and
// optional pause with [WithBodyBytesWithheld] and [WithReleaseDelay].
//
// Withholding zero chooses no fixed tail size or pause. When all remaining
// positive body suffixes fit one record and have send credit, their DATA and
// END_STREAM frames share that final record. Otherwise tth2 advances
// intermediate bytes across streams towards similar fractions of their body
// lengths, without completing a stream, then checks again. Flow-control credit
// can limit that progress. A server may start work on headers or prefixes
// before the final record. The shared record is a wire-level completion gate,
// not an execution guarantee.
//
// HTTP/2 requires each HEADERS/CONTINUATION block to remain contiguous, so tth2
// completes every header block before starting the next and transfers DATA only
// after the shared header release. Response collection is already active at
// that release because a peer may dispatch greedily upon receiving HEADERS.
//
// Trials expose two outbound wall-clock observations in [Trial.Dispatch]:
//
//	Batch shape                 HeadersSentAt     BodyFinalisedAt
//	bodyless                   HEADERS release   zero
//	explicit empty bodies      HEADERS release   zero
//	unwithheld body bytes      HEADERS release   final DATA release
//	withheld body tails        HEADERS release   tail/END_STREAM release
//	mixed bodyless and bodies  HEADERS release   final DATA release
//
// The header observation represents the earliest point at which a greedy peer
// could act on every request. The body-finalisation observation represents when
// every body-carrying request became complete. These timestamps are diagnostic
// metadata only. Infer timing differences from response arrival order, not from
// the timestamps or elapsed wall-clock durations.
//
// By default, tth2 honours SETTINGS_MAX_CONCURRENT_STREAMS and rejects a batch
// requiring more streams before it sends request headers. Each request uses one
// stream, and a batch is never divided because that would destroy its shared
// release gate. [WithPeerStreamLimitIgnored] deliberately exceeds the
// advertised limit for peer-enforcement testing.
//
// # Trials
//
// [Client.StreamTrials] repeats a batch and yields each [Trial] as it
// completes. [Client.RunTrials] drains a bounded stream into a [TrialSummary].
// Requests rotate through stream positions by default so complete cycles
// balance request identity across positions; [WithArrangementPolicy] selects
// other designs. StreamTrials accepts response capture options because its
// caller owns each yielded trial. RunTrials is summary-only and accepts only
// [RunTrialsOption], which excludes response capture.
//
// A trial runs entirely on one connection. Runs can use several pooled
// connections concurrently, and [Trial.Conn] identifies the connection within
// the run; a replacement receives a new ID. [BatchResult.Connection] identifies
// the physical connection within its Transport lifetime. Failures can split a
// rotation cycle across a failed connection and its primed replacement.
// Consumers requiring complete, connection-homogeneous cycles can group
// streamed trials by [Trial.Cycle] and [Trial.Conn].
//
// Each acquired connection is primed before measured work. If a connection
// dies during priming, its worker acquires one replacement and restarts the
// configured priming sequence. If the replacement cannot reach measured
// dispatch, the run ends through [TrialStream.Err] without consuming the
// waiting measured trial. The event ends once the connection successfully
// dispatches that batch. Retirement after completed priming but before dispatch
// therefore moves the measured work to a replacement under the same allowance.
// A later connection loss after measured dispatch starts a new event. Priming
// observations are yielded as trials with [Trial.Warmup] set, including
// failures. A priming failure that leaves the connection usable consumes its
// attempt but does not require a replacement.
//
// Summary counts are exact snapshots. Repeatedly testing a statistic and
// stopping when it crosses a threshold can still invalidate that test's error
// guarantees; see [TrialStream.Summary].
//
// # Options and bounds
//
// Option values may be shared across concurrent calls. Within each setting, the
// last option wins. A nil option panics. Send options also implement
// [TrialsOption]; response capture is excluded from [RunTrialsOption].
//
//	Setting                       SendBatch  StreamTrials  RunTrials
//	Body release and padding       yes        yes           yes
//	Batch timeout and body limit   yes        yes           yes
//	Response capture               yes        yes           no
//	Arrangement, warmup and pacing no         yes           yes
//	Progress callback              no         yes           yes
//	Measured trial bound           no         yes           argument
//
// Zero response capture retains nothing; zero accepted-body limit or timeout
// adds no limit. [UnlimitedResponseBytes] is accepted only by response capture
// and Transport.MaxResponseHeaderBytes. Header limits have a finite default.
// Request bodies are buffered in full per attempt; no package option bounds
// their size. Run summaries retain O(n*n) counters for n requests, independent
// of trial count. Stream consumers own and bound their retained results.
//
// # Requests and responses
//
// Requests are authoritative. tth2 performs the HTTP/2 wire translation:
// pseudo-headers, content length, HPACK, lowercase field names, and omission of
// forbidden hop-by-hop fields. It does not add content policy such as a default
// User-Agent or Accept header, authentication, cookies, redirects, or content
// sniffing. [PseudoHeaders] exposes pseudo-header translation. The encoder
// omits connection-specific fields without mutating request headers.
//
// Request bodies must be reusable because trials, stale-connection retries, and
// concurrent calls may read them repeatedly. Create requests with
// bytes.NewReader or strings.NewReader, or supply GetBody explicitly. Response
// bodies are drained and hashed unless [WithMaxResponseBodyBytes] stops the
// batch at an accepted per-stream prefix. Capture is configured independently
// with [WithResponseBodyCaptureBytes]. A finite capture limit bounds only the
// retained prefix of each accepted response, not transfer, hashing work,
// duration, total memory use, or caller retention of returned results.
//
// [Transport.MaxResponseHeaderBytes] limits every decoded final, informational,
// or trailer section independently of capture. Without
// [WithResponseHeaderCapture], responses retain status and unambiguous declared
// length but no header or trailer maps. Capture retains owned maps for accepted
// final headers and trailers; informational headers remain provisional.
//
// tth2 disables HTTP/2 server push because pushed responses have no request
// position in the request batch and therefore cannot contribute valid
// arrival evidence. A peer that sends PUSH_PROMISE despite that setting makes
// the connection unusable.
//
// A [Result] has three response states. An ordinary response has Response set
// and Reset nil. A reset before final response headers has Response nil,
// ArrivalRank -1, and Reset set. A reset after final headers has both Response
// and Reset set, retains its rank, and may have a truncated body. Use
// [Result.Arrived], [BatchResult.AllResponsesArrived], and
// [BatchResult.FullArrivalOrder] instead of reconstructing these rules.
//
// # Concurrency, pacing, and cancellation
//
// A [Client] and [Transport] are safe for concurrent use. The Transport pools
// connections per origin. Per-transport and per-run pacing options gate batch
// starts without changing spacing inside a batch. [Transport.Ping] measures a
// connection round trip without consuming request or batch pacing allowance.
//
// After the connection preface, one reader services each connection for its
// entire lifetime, including while it is pooled idle or held by a trial worker
// or [ConnLease] between batches. It applies and acknowledges SETTINGS in
// arrival order, acknowledges peer PINGs, tracks flow-control credit, and
// retires a connection on GOAWAY or a read failure. Frame writes are
// serialised: control frames may be sent between load-bearing TLS records, but
// never inserted into a batch-release record containing request headers or
// final body bytes.
//
// Context cancellation interrupts blocked sends and reads, returns without a
// retry, and discards the affected connection. [WithBatchTimeout] adds an
// independent per-batch wall-clock limit across dispatch, release delay, and
// response collection. The first of caller cancellation, local timeout, body
// overflow, peer reset, or transport failure determines the returned cause. A
// local timeout or body overflow returns no partial BatchResult and disposes of
// the connection synchronously. A connection-fatal failure on a pooled
// connection is treated as stale and retried; a failed fresh connection is not
// retried. A retry may repeat a partly dispatched batch, including
// non-idempotent methods. [StreamError] identifies one RST_STREAM. During
// response collection it preserves sibling results and connection reuse; during
// dispatch it returns no batch and disposes of the connection. Inspect both the
// error and result. GetBody and request body readers must return promptly
// because their synchronous calls cannot be interrupted by context.
package tth2
