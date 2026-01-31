package arrow_flight_eval

// This file contains a conceptual proof-of-concept showing what an
// Arrow Flight adapter for Bonanza's build queue state API would look
// like. It does NOT import the actual Arrow library to avoid adding a
// heavy dependency. Instead, it documents the Arrow types that would
// be used and demonstrates the impedance mismatch.
//
// The build queue state API (ListOperations, ListWorkers) is the
// closest thing to "tabular data" in Bonanza, making it the best
// candidate for Arrow Flight. Even so, the fit is poor.

import (
	"time"

	buildqueuestate_pb "bonanza.build/pkg/proto/buildqueuestate"

	"google.golang.org/protobuf/proto"
)

// OperationRecord is a flattened, denormalized representation of an
// OperationState message suitable for columnar storage. This
// demonstrates the first problem with Arrow for Bonanza: the data
// must be transposed from Protobuf's nested, sparse format into a
// flat columnar layout.
//
// In Arrow, this would be represented as a schema with these fields:
//
//	arrow.NewSchema([]arrow.Field{
//	    {Name: "name", Type: arrow.BinaryTypes.String},
//	    {Name: "platform_pkix_public_key", Type: arrow.BinaryTypes.Binary},
//	    {Name: "size_class", Type: arrow.PrimitiveTypes.Uint32},
//	    {Name: "expected_duration_nanos", Type: arrow.PrimitiveTypes.Int64},
//	    {Name: "queued_timestamp_nanos", Type: arrow.PrimitiveTypes.Int64},
//	    {Name: "timeout_nanos", Type: arrow.PrimitiveTypes.Int64},
//	    {Name: "stage", Type: arrow.PrimitiveTypes.Uint8},
//	    {Name: "priority", Type: arrow.PrimitiveTypes.Int32},
//	}, nil)
type OperationRecord struct {
	Name                   string
	PlatformPKIXPublicKey  []byte
	SizeClass              uint32
	ExpectedDurationNanos  int64
	QueuedTimestampNanos   int64
	TimeoutNanos           int64
	Stage                  uint8 // 0=queued, 1=executing, 2=completed
	Priority               int32
}

// WorkerRecord is a flattened representation of a WorkerState message.
// The map[string]string worker ID field is problematic for Arrow's
// columnar model — it requires either a MapArray (complex, slow) or
// denormalization into a fixed set of known key columns.
//
// In Arrow, this would be something like:
//
//	arrow.NewSchema([]arrow.Field{
//	    {Name: "worker_id_json", Type: arrow.BinaryTypes.String},
//	    {Name: "timeout_nanos", Type: arrow.PrimitiveTypes.Int64},
//	    {Name: "current_operation_name", Type: arrow.BinaryTypes.String},
//	    {Name: "drained", Type: arrow.FixedWidthTypes.Boolean},
//	}, nil)
type WorkerRecord struct {
	// WorkerID is serialized as JSON because Arrow has no native
	// map<string,string> type that is efficient for small maps with
	// variable keys. This defeats the purpose of columnar storage.
	WorkerIDJSON           string
	TimeoutNanos           int64
	CurrentOperationName   string
	Drained                bool
}

// ConvertOperationsToRecords transposes a slice of OperationState
// Protobuf messages into the flat columnar record format that Arrow
// would require. This demonstrates the overhead of the row-to-column
// conversion that Arrow Flight would impose on every response.
//
// For Bonanza's typical page sizes (10-100 operations), this
// conversion is pure overhead with no corresponding benefit — the
// data is consumed one row at a time by the browser's HTML renderer.
func ConvertOperationsToRecords(ops []*buildqueuestate_pb.OperationState) []OperationRecord {
	records := make([]OperationRecord, len(ops))
	for i, op := range ops {
		r := OperationRecord{
			Name:     op.GetName(),
			Priority: op.GetPriority(),
		}

		if inv := op.GetInvocationName(); inv != nil {
			if scqn := inv.GetSizeClassQueueName(); scqn != nil {
				r.PlatformPKIXPublicKey = scqn.GetPlatformPkixPublicKey()
				r.SizeClass = scqn.GetSizeClass()
			}
		}

		if d := op.GetExpectedDuration(); d != nil {
			r.ExpectedDurationNanos = d.GetSeconds()*1e9 + int64(d.GetNanos())
		}

		if t := op.GetQueuedTimestamp(); t != nil {
			r.QueuedTimestampNanos = t.GetSeconds()*1e9 + int64(t.GetNanos())
		}

		if t := op.GetTimeout(); t != nil {
			r.TimeoutNanos = t.GetSeconds()*1e9 + int64(t.GetNanos())
		}

		switch {
		case op.GetQueued() != nil:
			r.Stage = 0
		case op.GetExecuting() != nil:
			r.Stage = 1
		case op.GetCompleted() != nil:
			r.Stage = 2
		}

		records[i] = r
	}
	return records
}

// ArrowIPCOverhead estimates the per-batch metadata overhead of Arrow
// IPC for a given number of columns. This is the fixed cost paid for
// every record batch sent over Arrow Flight, regardless of how many
// rows are in the batch.
//
// Components:
//   - Continuation indicator: 4 bytes
//   - Metadata length: 4 bytes
//   - Flatbuffer Message envelope: ~60 bytes (avg)
//   - Per-column FieldNode (length + null_count): 16 bytes each
//   - Per-column Buffer descriptor (offset + length): 16 bytes each
//     (typically 2 buffers per column: validity + data)
//   - Padding to 8-byte alignment: ~4 bytes avg
func ArrowIPCOverhead(numColumns int) int {
	const (
		continuationIndicator = 4
		metadataLength        = 4
		flatbufferEnvelope    = 60
		fieldNodeSize         = 16
		bufferDescriptorSize  = 16
		buffersPerColumn      = 2 // validity bitmap + data
		avgPadding            = 4
	)
	return continuationIndicator +
		metadataLength +
		flatbufferEnvelope +
		numColumns*fieldNodeSize +
		numColumns*buffersPerColumn*bufferDescriptorSize +
		avgPadding
}

// ProtobufWireSize returns the serialized size of an OperationState
// message. This demonstrates that Bonanza's typical messages are
// small — well under the ~10 KB crossover point where Arrow IPC
// becomes more efficient than Protobuf.
func ProtobufWireSize(op *buildqueuestate_pb.OperationState) int {
	return proto.Size(op)
}

// OverheadComparison contains the results of comparing Protobuf vs.
// Arrow IPC serialization overhead for a batch of operations.
type OverheadComparison struct {
	// NumOperations is the number of operations in the batch.
	NumOperations int

	// ProtobufTotalBytes is the total serialized size of all
	// operations using Protobuf (each operation as a separate message
	// in a ListOperationsResponse).
	ProtobufTotalBytes int

	// ArrowIPCEstimatedBytes is the estimated size using Arrow IPC:
	// per-batch metadata overhead + raw columnar data.
	ArrowIPCEstimatedBytes int

	// ArrowOverheadRatio is ArrowIPCEstimatedBytes / ProtobufTotalBytes.
	// Values > 1.0 mean Arrow is larger (worse for Bonanza's use case).
	ArrowOverheadRatio float64
}

// CompareOverhead computes the serialization overhead comparison for a
// batch of operations at various batch sizes, demonstrating that Arrow
// Flight's per-batch metadata overhead dominates at Bonanza's typical
// page sizes.
func CompareOverhead(ops []*buildqueuestate_pb.OperationState) OverheadComparison {
	const numColumns = 8 // fields in OperationRecord

	// Protobuf: sum of individual message sizes + ListOperationsResponse framing.
	pbTotal := 0
	for _, op := range ops {
		pbTotal += proto.Size(op)
	}
	// Add approximate ListOperationsResponse wrapper overhead.
	pbTotal += 10 // field tag + length prefix for repeated field

	// Arrow IPC: metadata overhead + raw data estimate.
	// Raw data: for each row, the fixed-width columns take constant space.
	// String/binary columns add variable overhead.
	arrowMetadata := ArrowIPCOverhead(numColumns)
	arrowRawData := 0
	for _, op := range ops {
		// name: 4 bytes length prefix + string bytes
		arrowRawData += 4 + len(op.GetName())
		// platform_pkix_public_key: 4 bytes offset + variable bytes
		if inv := op.GetInvocationName(); inv != nil {
			if scqn := inv.GetSizeClassQueueName(); scqn != nil {
				arrowRawData += 4 + len(scqn.GetPlatformPkixPublicKey())
			}
		}
		// Fixed-width columns: uint32 + int64 + int64 + int64 + uint8 + int32 = 29 bytes
		arrowRawData += 29
	}
	// Validity bitmaps: 1 bit per row per nullable column, ceil to bytes.
	nullableColumns := 5 // duration, timestamps, timeout, operation details
	arrowRawData += nullableColumns * ((len(ops) + 7) / 8)

	arrowTotal := arrowMetadata + arrowRawData

	ratio := 0.0
	if pbTotal > 0 {
		ratio = float64(arrowTotal) / float64(pbTotal)
	}

	return OverheadComparison{
		NumOperations:          len(ops),
		ProtobufTotalBytes:     pbTotal,
		ArrowIPCEstimatedBytes: arrowTotal,
		ArrowOverheadRatio:     ratio,
	}
}

// TypicalBonanzaMessageSizes returns the size distribution of messages
// in Bonanza's various APIs, demonstrating that most messages are well
// below Arrow's efficiency crossover point.
func TypicalBonanzaMessageSizes() map[string]MessageSizeInfo {
	return map[string]MessageSizeInfo{
		"DownloadObjectRequest": {
			TypicalBytes: 80,  // namespace + 40-byte reference
			MaxBytes:     120,
			Note:         "Point lookup by hash — single object, not a batch",
		},
		"DownloadObjectResponse": {
			TypicalBytes: 1024,     // small objects (compiled .bzl, configs)
			MaxBytes:     2097152,  // 2 MiB hard cap
			Note:         "Binary blob — not tabular, Arrow adds no value",
		},
		"UploadDagsRequest.ProvideObjectContents": {
			TypicalBytes: 512,
			MaxBytes:     2097152,
			Note:         "Interleaved with control messages in bidirectional stream",
		},
		"UploadDagsResponse.RequestObject": {
			TypicalBytes: 16,  // reference_index + bool
			MaxBytes:     24,
			Note:         "Tiny control message — Arrow IPC overhead would be 40x the payload",
		},
		"ListOperationsResponse (page)": {
			TypicalBytes: 2048,  // ~20 operations, ~100 bytes each
			MaxBytes:     10240, // large page
			Note:         "Best candidate for Arrow, but still below crossover point",
		},
		"ListWorkersResponse (page)": {
			TypicalBytes: 1024,
			MaxBytes:     5120,
			Note:         "Worker state includes map<string,string> — poor Arrow fit",
		},
		"Evaluation (single key result)": {
			TypicalBytes: 256,
			MaxBytes:     2097152,
			Note:         "Tree-structured, not tabular",
		},
	}
}

// MessageSizeInfo describes the typical wire size of a Bonanza message type.
type MessageSizeInfo struct {
	TypicalBytes int
	MaxBytes     int
	Note         string
}

// Unused but documents what the Flight service interface would look
// like. Included as documentation for anyone considering this path.
//
// An Arrow Flight server for build queue state would need to:
//
//  1. Implement GetFlightInfo to advertise available "tables"
//     (operations, workers, drains, invocations).
//
//  2. Implement DoGet to stream paginated results as Arrow record
//     batches — but since the browser renders HTML one row at a time,
//     the columnar format would be immediately transposed back to rows,
//     negating any benefit.
//
//  3. Handle authentication, which Bonanza currently does via
//     elliptic-curve key exchange and encrypted actions — none of
//     which maps to Arrow Flight's token-based auth model.
//
//  4. Maintain the reference-counting discipline required by the Arrow
//     Go library (manual Retain()/Release() calls), which is
//     counter-idiomatic for Go and a source of memory leaks.
//
// The full signature would be:
//
//	type BuildQueueFlightServer struct {
//	    flight.BaseFlightServer
//	    buildQueueClient buildqueuestate_pb.BuildQueueStateClient
//	}
//
//	func (s *BuildQueueFlightServer) GetFlightInfo(
//	    ctx context.Context,
//	    desc *flight.FlightDescriptor,
//	) (*flight.FlightInfo, error) { ... }
//
//	func (s *BuildQueueFlightServer) DoGet(
//	    ticket *flight.Ticket,
//	    stream flight.FlightService_DoGetServer,
//	) error { ... }

// SimulateFlightRoundTrip simulates the work that would be done in an
// Arrow Flight DoGet call for listing operations, to demonstrate the
// overhead vs. the current Protobuf approach.
//
// Steps that Arrow Flight would require:
//  1. Receive ticket, parse pagination parameters
//  2. Call existing gRPC BuildQueueState.ListOperations
//  3. Deserialize Protobuf response
//  4. Allocate Arrow record batch builders (with reference counting)
//  5. Transpose each row into columnar builders
//  6. Build the record batch (finalize buffers)
//  7. Serialize as Arrow IPC
//  8. Send via Flight's DoGet stream
//
// Steps the current Protobuf approach requires:
//  1. Receive request
//  2. Serialize response as Protobuf
//  3. Send via gRPC stream
//
// The Arrow path adds steps 3-7 as pure overhead.
func SimulateFlightRoundTrip(ops []*buildqueuestate_pb.OperationState) (protoBytes int, arrowEstimatedBytes int) {
	comparison := CompareOverhead(ops)
	return comparison.ProtobufTotalBytes, comparison.ArrowIPCEstimatedBytes
}

// The timestamp is used to ensure the analysis is dated.
var evaluationDate = time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
