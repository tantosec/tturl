package timing

import (
	"crypto/tls"
	"net/http"
	"time"
)

// Protocol selects one HTTP protocol without fallback.
type Protocol string

const (
	// HTTP2 selects HTTPS HTTP/2.
	HTTP2 Protocol = "h2"
	// HTTP11 selects HTTP/1.1 over HTTP or HTTPS.
	HTTP11 Protocol = "http/1.1"
)

// Arrangement assigns request identities to outbound positions.
type Arrangement string

const (
	// ArrangeNone retains authored order.
	ArrangeNone Arrangement = "none"
	// ArrangeRotate executes complete cycles of position rotations.
	ArrangeRotate Arrangement = "rotate"
	// ArrangeRandom independently permutes each trial.
	ArrangeRandom Arrangement = "random"
)

// Request contains materialised metadata and immutable body bytes.
// The engine never reads HTTP.Body. ID is stable across trials and priming.
type Request struct {
	ID       int
	HTTP     *http.Request
	Body     []byte
	Protocol Protocol
}

// Capture selects retained evidence independently of response draining.
// BodyMax is a prefix limit; -1 means unlimited when Body is enabled.
type Capture struct {
	Headers bool
	Body    bool
	BodyMax int64
}

// Plan supplies explicit policies. Zero trials means unlimited; zero rates
// and timeouts remove finite limits. Zero connections derives one worker.
// ResponseBodyMax uses -1 for unlimited. ReceiveHeaderMax is finite.
// Priming nil selects Requests; arrangement applies to either warmup set.
type Plan struct {
	Requests         []Request
	Priming          []Request
	Trials           uint64
	Arrangement      Arrangement
	Warmup           int
	Connections      int
	Synchronise      bool
	SingleRecord     bool
	LastByteSync     bool
	ReleaseDelay     time.Duration
	RequestTimeout   time.Duration
	RunTimeout       time.Duration
	BatchRate        float64
	RequestRate      float64
	ReceiveHeaderMax int64
	ResponseBodyMax  int64
	Capture          Capture
	TLSConfig        *tls.Config
}

// OptionalInt distinguishes an absent declaration from a declared zero.
type OptionalInt struct {
	Value   int64
	Present bool
}

// RequestInfo describes validated materialised request framing.
type RequestInfo struct {
	ID                    int
	PoolID                int
	Protocol              Protocol
	Method, URL           string
	BodyBytes             int64
	DeclaredContentLength OptionalInt
	LengthMismatch        bool
}

// Pool describes a compatible destination, independently of HTTP authority.
type Pool struct {
	ID                 int
	Scheme, Host, Port string
	Address            string
	Protocol           Protocol
	RequestIDs         []int
	PrimingRequestIDs  []int
	Width              int
}

// Resolved is an immutable validated plan and complete-worker allocation.
// Resolve owns all snapshots; callers must not mutate a resolved plan.
type Resolved struct {
	Plan                     Plan
	Pools                    []Pool
	Requests                 []RequestInfo
	Priming                  []RequestInfo
	Width                    int
	Destinations             int
	RequestedTrials          uint64
	PlannedTrials            uint64
	WorkUnits                uint64
	ConnectionCeiling        int
	ConnectionsPerWorker     int
	Workers                  int
	EffectiveConnections     int
	PlannedRequestOperations uint64
	WarmupWidth              int
	PlannedWarmupTrials      uint64
	PlannedWarmupOperations  uint64
}

// Phase identifies the lifetime in which an operational failure occurred.
type Phase string

const (
	// PhaseAcquisition covers DNS, TCP, TLS and protocol setup.
	PhaseAcquisition Phase = "acquisition"
	// PhaseReadiness covers preparation through actual initial release.
	PhaseReadiness Phase = "readiness"
	// PhaseExecution covers actual initial release through delivery and drain.
	PhaseExecution Phase = "execution"
	// PhaseOutput covers result delivery.
	PhaseOutput Phase = "output"
	// PhaseRun covers campaign cancellation and run limits.
	PhaseRun Phase = "run"
)

// Failure retains classification separately from observed protocol events.
type Failure struct {
	Phase   Phase
	Code    string
	Message string
	Cause   error
}

// Error describes the operational failure without formatting an observation.
func (f *Failure) Error() string {
	if f.Message != "" {
		return f.Message
	}
	if f.Cause != nil {
		return f.Cause.Error()
	}
	return f.Code
}

// Unwrap preserves cancellation, deadline and protocol error identity.
func (f *Failure) Unwrap() error { return f.Cause }

// Offset is a signed monotonic nanosecond value or explicit absence.
type Offset struct {
	NS      int64
	Present bool
	Reason  string
}

// Timing retains six milestone offsets and the primary header duration.
type Timing struct {
	InitialRelease       Offset
	FinalRelease         Offset
	WriteComplete        Offset
	FirstResponseHeaders Offset
	FinalResponseHeaders Offset
	ResponseComplete     Offset
	Duration             Offset
}

// Response owns retained evidence; Complete describes validated response drain.
// An observed protocol end can exist even when Complete is false.
type Response struct {
	Status                int
	DeclaredContentLength OptionalInt
	ReceivedBodyBytes     int64
	AcceptedBodyBytes     int64
	Digest                string
	LengthMismatch        bool
	Complete              bool
	Reset                 bool
	Truncated             bool
	Headers, Trailers     http.Header
	Body                  []byte
	CaptureTruncated      bool
}

// Outcome retains one planned request, including unreleased work.
// Physical connection IDs start at one; zero means no assigned connection.
type Outcome struct {
	RequestID     int
	Position      int
	PoolID        int
	WorkerID      int
	ConnectionID  uint64
	Protocol      Protocol
	Attempted     bool
	EarlyResponse bool
	Timing        Timing
	Response      Response
	Failure       *Failure
}

// Connection records physical identity and acquisition or retirement evidence.
type Connection struct {
	ID                 uint64
	WorkerID, PoolID   int
	Slot               int
	ReplacementOf      uint64
	Protocol           Protocol
	NegotiatedProtocol string
	LocalAddress       string
	RemoteAddress      string
	State              string
	Reason             string
	Failure            *Failure
}

// Trial owns one complete planned membership in actual position order.
type Trial struct {
	Index             uint64
	WorkerID          int
	StartedAt         time.Time
	InitialGate       Offset
	FinalGate         Offset
	Order             []int
	Outcomes          []Outcome
	Committed         bool
	OfferedOperations uint64
	Complete          bool
	Failure           *Failure
}

// Warmup records one whole unmeasured worker trial.
type Warmup struct {
	WorkerID          int
	Phase             string
	PhaseIndex        int
	TrialIndex        int
	StartedAt         time.Time
	InitialGate       Offset
	FinalGate         Offset
	Order             []int
	Outcomes          []Outcome
	Committed         bool
	OfferedOperations uint64
	Complete          bool
	Failure           *Failure
}

// PhaseCompletion records a successfully checked lifecycle boundary.
type PhaseCompletion struct {
	Phase      string
	WorkerID   int
	PhaseIndex int
}

// Event contains one phase completion, connection, warmup or measured trial.
// Run delivers owned events serially with bounded backpressure.
type Event struct {
	Phase      *PhaseCompletion
	Connection *Connection
	Warmup     *Warmup
	Trial      *Trial
}

// Accounting reconciles planned membership, actual work and committed cost.
type Accounting struct {
	TrialsAttempted              uint64
	TrialsComplete               uint64
	TrialsIncomplete             uint64
	TrialsUnattempted            uint64
	CyclesAttempted              uint64
	CyclesComplete               uint64
	CyclesIncomplete             uint64
	CyclesUnattempted            uint64
	RequestOperationsAttempted   uint64
	RequestOperationsComplete    uint64
	RequestOperationsFailed      uint64
	RequestOperationsUnattempted uint64
	WarmupTrials                 uint64
	WarmupTrialsComplete         uint64
	WarmupTrialsIncomplete       uint64
	WarmupOperationsAttempted    uint64
	WarmupOperationsComplete     uint64
	WarmupOperationsFailed       uint64
	WarmupOperationsUnattempted  uint64
	InitialWarmupTrials          uint64
	ReplacementWarmupTrials      uint64
	MeasuredCommitments          uint64
	WarmupCommitments            uint64
	MeasuredOfferedOperations    uint64
	WarmupOfferedOperations      uint64
	ConnectionsAcquired          uint64
	ConnectionsRetired           uint64
	ConnectionsReplaced          uint64
	AcquisitionFailures          uint64
}

// Completion identifies bounded completion, fulfilled unbounded stop
// or failure.
type Completion string

const (
	// CompletionComplete means all planned trials were attempted.
	CompletionComplete Completion = "complete"
	// CompletionStopped means a user stop fulfilled an unbounded run.
	CompletionStopped Completion = "stopped"
	// CompletionFailed means fatal failure or unfinished bounded work.
	CompletionFailed Completion = "failed"
)

// RequestAccounting retains actual measured work for one stable request ID.
type RequestAccounting struct {
	RequestID   int
	Attempted   uint64
	Unattempted uint64
}

// Result retains terminal accounting, never full observations or body samples.
type Result struct {
	StartedAt  time.Time
	Completion Completion
	Accounting Accounting
	Requests   []RequestAccounting
	Failure    *Failure
}
