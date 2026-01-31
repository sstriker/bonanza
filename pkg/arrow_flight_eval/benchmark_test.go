package arrow_flight_eval

import (
	"fmt"
	"testing"

	buildqueuestate_pb "bonanza.build/pkg/proto/buildqueuestate"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// makeOperations creates a slice of realistic OperationState messages
// for benchmarking. These mirror what bonanza_scheduler would produce.
func makeOperations(n int) []*buildqueuestate_pb.OperationState {
	ops := make([]*buildqueuestate_pb.OperationState, n)
	for i := range n {
		op := &buildqueuestate_pb.OperationState{
			Name: fmt.Sprintf("operation-%08d", i),
			InvocationName: &buildqueuestate_pb.InvocationName{
				SizeClassQueueName: &buildqueuestate_pb.SizeClassQueueName{
					PlatformPkixPublicKey: make([]byte, 91), // typical ECDSA P-256 DER key
					SizeClass:             1,
				},
			},
			ExpectedDuration: &durationpb.Duration{
				Seconds: 30,
			},
			QueuedTimestamp: &timestamppb.Timestamp{
				Seconds: 1706745600 + int64(i),
			},
			Priority: int32(i % 10),
		}

		// Distribute across stages.
		switch i % 3 {
		case 0:
			op.Stage = &buildqueuestate_pb.OperationState_Queued{
				Queued: &emptypb.Empty{},
			}
		case 1:
			op.Stage = &buildqueuestate_pb.OperationState_Executing{
				Executing: &emptypb.Empty{},
			}
		case 2:
			op.Stage = &buildqueuestate_pb.OperationState_Completed{
				Completed: &emptypb.Empty{},
			}
		}

		ops[i] = op
	}
	return ops
}

// BenchmarkProtobufMarshalSingle measures the cost of serializing a
// single OperationState message, which is the fundamental unit of work
// for Bonanza's build queue APIs.
func BenchmarkProtobufMarshalSingle(b *testing.B) {
	op := makeOperations(1)[0]
	b.ResetTimer()
	for range b.N {
		data, err := proto.Marshal(op)
		if err != nil {
			b.Fatal(err)
		}
		_ = data
	}
}

// BenchmarkProtobufUnmarshalSingle measures deserialization cost.
func BenchmarkProtobufUnmarshalSingle(b *testing.B) {
	op := makeOperations(1)[0]
	data, err := proto.Marshal(op)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		var msg buildqueuestate_pb.OperationState
		if err := proto.Unmarshal(data, &msg); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkProtobufMarshalBatch measures the cost of serializing a
// ListOperationsResponse containing N operations.
func BenchmarkProtobufMarshalBatch(b *testing.B) {
	for _, batchSize := range []int{1, 10, 50, 100, 500} {
		b.Run(fmt.Sprintf("batch_%d", batchSize), func(b *testing.B) {
			ops := makeOperations(batchSize)
			resp := &buildqueuestate_pb.ListOperationsResponse{
				Operations: ops,
				PaginationInfo: &buildqueuestate_pb.PaginationInfo{
					StartIndex:   0,
					TotalEntries: uint32(batchSize * 10),
				},
			}
			b.ResetTimer()
			for range b.N {
				data, err := proto.Marshal(resp)
				if err != nil {
					b.Fatal(err)
				}
				_ = data
			}
		})
	}
}

// BenchmarkProtobufUnmarshalBatch measures deserialization cost for batches.
func BenchmarkProtobufUnmarshalBatch(b *testing.B) {
	for _, batchSize := range []int{1, 10, 50, 100, 500} {
		b.Run(fmt.Sprintf("batch_%d", batchSize), func(b *testing.B) {
			ops := makeOperations(batchSize)
			resp := &buildqueuestate_pb.ListOperationsResponse{
				Operations: ops,
				PaginationInfo: &buildqueuestate_pb.PaginationInfo{
					StartIndex:   0,
					TotalEntries: uint32(batchSize * 10),
				},
			}
			data, err := proto.Marshal(resp)
			if err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for range b.N {
				var msg buildqueuestate_pb.ListOperationsResponse
				if err := proto.Unmarshal(data, &msg); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRowToColumnConversion measures the overhead of transposing
// Protobuf messages into a flat columnar format, which Arrow Flight
// would require on every response.
func BenchmarkRowToColumnConversion(b *testing.B) {
	for _, batchSize := range []int{1, 10, 50, 100, 500} {
		b.Run(fmt.Sprintf("batch_%d", batchSize), func(b *testing.B) {
			ops := makeOperations(batchSize)
			b.ResetTimer()
			for range b.N {
				records := ConvertOperationsToRecords(ops)
				_ = records
			}
		})
	}
}

// TestOverheadComparison prints the overhead comparison table to
// demonstrate that Arrow IPC metadata dominates at Bonanza's typical
// batch sizes.
func TestOverheadComparison(t *testing.T) {
	t.Log("Protobuf vs Arrow IPC overhead comparison for OperationState batches")
	t.Log("====================================================================")
	t.Logf("%-10s  %-12s  %-12s  %-10s", "Batch Size", "Protobuf (B)", "Arrow IPC (B)", "Ratio")
	t.Log("----------  ------------  -------------  ----------")

	for _, batchSize := range []int{1, 5, 10, 20, 50, 100, 500} {
		ops := makeOperations(batchSize)
		cmp := CompareOverhead(ops)
		t.Logf("%-10d  %-12d  %-12d  %-10.2f",
			cmp.NumOperations,
			cmp.ProtobufTotalBytes,
			cmp.ArrowIPCEstimatedBytes,
			cmp.ArrowOverheadRatio,
		)
	}

	t.Log("")
	t.Log("Ratio > 1.0 means Arrow IPC is LARGER than Protobuf (worse).")
	t.Log("Arrow Flight only becomes more efficient at very large batch sizes")
	t.Log("(thousands of rows), which Bonanza's paginated APIs never produce.")
}

// TestTypicalMessageSizes prints the typical message sizes for Bonanza's
// APIs to demonstrate that most are well below Arrow's crossover point.
func TestTypicalMessageSizes(t *testing.T) {
	t.Log("Typical message sizes in Bonanza APIs")
	t.Log("======================================")
	t.Logf("%-50s  %-12s  %-12s  %s", "Message Type", "Typical (B)", "Max (B)", "Note")
	t.Log("--------------------------------------------------  ------------  ------------  ----")

	sizes := TypicalBonanzaMessageSizes()
	for name, info := range sizes {
		t.Logf("%-50s  %-12d  %-12d  %s", name, info.TypicalBytes, info.MaxBytes, info.Note)
	}

	t.Log("")
	t.Log("Arrow IPC metadata overhead per batch: ~460 bytes (8 columns)")
	t.Logf("Arrow IPC overhead = %d bytes", ArrowIPCOverhead(8))
	t.Log("Most Bonanza messages are smaller than the Arrow metadata overhead alone.")
}

// TestSingleOperationWireSize measures the actual Protobuf wire size of
// a typical OperationState message, to anchor the overhead comparison
// in real data.
func TestSingleOperationWireSize(t *testing.T) {
	ops := makeOperations(1)
	data, err := proto.Marshal(ops[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Single OperationState Protobuf wire size: %d bytes", len(data))
	t.Logf("Arrow IPC overhead for 8-column batch:    %d bytes", ArrowIPCOverhead(8))
	t.Logf("Arrow IPC overhead for 1 row is %.1fx the Protobuf size",
		float64(ArrowIPCOverhead(8))/float64(len(data)))
}
