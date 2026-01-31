// Package arrow_flight_eval contains a proof-of-concept evaluation of
// Apache Arrow Flight for Bonanza's storage and API layer.
//
// This package demonstrates:
//
//   - What an Arrow Flight adapter for Bonanza's build queue state
//     would look like conceptually (adapter.go)
//   - Benchmark comparisons of Protobuf serialization vs. simulated
//     Arrow IPC overhead for Bonanza's typical data shapes (benchmark_test.go)
//
// Conclusion: Arrow Flight is not a good fit for Bonanza. See
// doc/evaluations/arrow-flight.md for the full analysis.
package arrow_flight_eval
