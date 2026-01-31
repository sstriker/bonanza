# Evaluation: Apache Arrow Flight for Bonanza Storage and APIs

## Summary

**Recommendation: Do not adopt Apache Arrow Flight for Bonanza's storage or API layer.**

Arrow Flight is designed for high-throughput columnar data transfer between
analytics systems. Bonanza's data access patterns — small, heterogeneous,
graph-structured messages transferred via fine-grained bidirectional streaming —
are the precise anti-pattern for Arrow Flight. Adopting it would add complexity
and overhead with no measurable benefit.

## What is Apache Arrow Flight?

Apache Arrow Flight is an RPC framework built on gRPC that transfers data in
Apache Arrow's columnar in-memory format. It achieves high throughput (2-3+
GB/s) for large tabular datasets by:

- **Zero-copy transfer**: The on-wire representation matches the in-memory
  columnar format, eliminating serialization/deserialization.
- **Bypassing Protobuf for data payloads**: Only control messages use Protobuf;
  the actual data is Arrow IPC (Flatbuffer-framed columnar buffers).
- **Native parallel streams**: `GetFlightInfo` can return multiple endpoints for
  parallel data retrieval from a cluster.

Flight defines five data operations: `DoGet` (download), `DoPut` (upload),
`DoExchange` (bidirectional), `DoAction` (custom actions), and
`ListFlights`/`GetFlightInfo` (discovery).

## Analysis: Bonanza's Data Patterns vs. Arrow's Strengths

### 1. Object Store (pkg/storage/object/)

| Aspect | Bonanza | Arrow ideal |
|--------|---------|-------------|
| Access granularity | Single object (1 B - 2 MiB) | Batches of 10K+ rows |
| Data shape | Opaque binary blobs with references | Typed, columnar tables |
| Operations | Point lookup by SHA-256 hash | Range scans, projections |
| Wire format | Protobuf: ~120 B overhead per message | Arrow IPC: ~600+ B overhead per batch |

**Verdict**: Arrow adds overhead. Objects are content-addressed binary blobs
retrieved individually. There is no batch dimension to exploit, and the data is
not tabular. Arrow IPC metadata overhead (Flatbuffer headers, field nodes,
buffer descriptors) would exceed the payload size for most objects.

### 2. DAG Upload Protocol (pkg/storage/dag/)

| Aspect | Bonanza | Arrow ideal |
|--------|---------|-------------|
| Protocol | Bidirectional streaming with interleaved handshakes | Unidirectional DoGet/DoPut |
| Message types | 6 distinct message types interleaved | Homogeneous record batches |
| Control flow | Per-object request/response (RequestObject → ProvideObjectContents) | Stream entire dataset |
| Deduplication | Server-driven, reference-index correlation | N/A |

**Verdict**: Fundamentally incompatible. The DAG upload protocol is a
fine-grained, stateful, bidirectional negotiation where the server decides
per-object whether to request contents. This maps to gRPC bidirectional
streaming with Protobuf oneof messages. Arrow Flight's `DoExchange` could
theoretically carry this, but the data payloads are individual objects (not
columnar batches), so Arrow adds complexity without reducing serialization cost.

### 3. Evaluation Model (pkg/model/evaluation/)

| Aspect | Bonanza | Arrow ideal |
|--------|---------|-------------|
| Data structure | B-tree of Graphlets | Flat table of records |
| Processing | Key-by-key evaluation | Vectorized column operations |
| Storage | Varint-delimited proto lists in 2 MiB objects | Arrow IPC files/streams |
| Access pattern | Recursive descent through references | Sequential scan |

**Verdict**: Poor fit. Evaluations are stored as B-trees of Graphlets, where
each node references child objects by cryptographic hash. Processing is
inherently key-by-key (evaluate → check dependencies → evaluate dependencies →
cache result). Arrow's strength is vectorized operations over columns of
homogeneous data, which does not apply here.

### 4. Build Queue State (pkg/proto/buildqueuestate/)

| Aspect | Bonanza | Arrow ideal |
|--------|---------|-------------|
| Data volume | Tens to hundreds of operations/workers | Millions of rows |
| Query pattern | Paginated list with server-side filtering | Analytical aggregation |
| Response size | Small pages (tens of items, ~KB each) | Large result sets (MB-GB) |
| Update frequency | Real-time state changes | Batch ETL |

**Verdict**: Marginal at best. Build queue state queries return small paginated
lists of operations and workers. The overhead of Arrow IPC metadata per batch
(~600 bytes for a 10-field schema) is significant relative to the data volume.
Protobuf is more efficient for these small, sparse messages.

### 5. Browser Service (cmd/bonanza_browser/)

| Aspect | Bonanza | Arrow ideal |
|--------|---------|-------------|
| Protocol | HTTP + HTML rendering | Arrow Flight (gRPC) |
| Access pattern | Navigate object graph by following references | Download large datasets |
| Response format | Rendered HTML pages | Arrow record batches |

**Verdict**: Not applicable. The browser service renders HTML pages for human
consumption, navigating the object graph one reference at a time. This is a
web application, not a data transfer service.

## Quantitative Overhead Analysis

For Bonanza's typical message sizes, Arrow IPC adds significant per-message
overhead compared to Protobuf:

### Per-message overhead comparison

| Payload size | Protobuf wire size | Arrow IPC wire size | Arrow overhead |
|--------------|-------------------|---------------------|----------------|
| 100 B | ~120-150 B | ~700-800 B | 5-7x |
| 1 KB | ~1.1 KB | ~1.6 KB | 1.5x |
| 10 KB | ~11 KB | ~10.6 KB | ~1x (crossover) |
| 1 MB | ~1.1-1.3 MB | ~1.001 MB | 0.8x (Arrow wins) |

Arrow IPC per-batch fixed costs:
- gRPC/HTTP2 frame: ~9 B
- Protobuf FlightData wrapper: ~20-50 B
- IPC continuation + metadata size: 8 B
- Flatbuffer Message envelope: ~40-80 B
- Per-column FieldNode: 16 B each
- Per-buffer descriptor: 16 B each
- Padding to 8-byte alignment: variable

For a schema with N columns: **~100 + 48*N bytes** of metadata overhead per
record batch. Most Bonanza messages are well under the 10 KB crossover point.

### Go-specific concerns

The Apache Arrow Go library (`github.com/apache/arrow-go/v18`) uses manual
reference counting (`Retain()`/`Release()`) for memory management. This is:

- Counter-idiomatic for Go (which uses garbage collection)
- Error-prone (leaks on missed `Release()`, use-after-free on early `Release()`)
- An ongoing maintenance burden for the team
- Not caught by Go's race detector or standard tooling

## Where Arrow Flight Would Add Value (Hypothetical)

Arrow Flight would become relevant if Bonanza's requirements shifted to include:

1. **Build analytics service**: A new service that queries across thousands of
   completed builds to compute statistics (average build time by target,
   flakiness rates, resource utilization trends). This would involve scanning
   large volumes of homogeneous, structured records — Arrow's sweet spot.

2. **Evaluation result export**: A bulk export API that allows external tools to
   download all evaluation results for a build as a Parquet file or Arrow stream
   for offline analysis.

3. **Metrics time series**: If Bonanza collected fine-grained worker metrics
   (CPU, memory, I/O per second) and needed to transfer these large time series
   to a monitoring system.

None of these are current requirements.

## Alternatives That Would Actually Help

If serialization performance is a concern, these alternatives are better suited
to Bonanza's data patterns:

1. **vtprotobuf** (already a dependency): Generates optimized Protobuf
   marshal/unmarshal code with pool-based allocation. This directly reduces
   serialization cost without changing the data model.

2. **Proto list lazy parsing**: For the varint-delimited proto lists in objects,
   implement lazy/partial deserialization that skips fields not needed by the
   consumer. This is essentially "column projection" within the existing format.

3. **Object caching with mmap**: For read-heavy patterns in the browser, memory-
   map frequently accessed objects to avoid repeated deserialization.

4. **gRPC stream batching**: For the DAG upload protocol, batch multiple small
   objects into single gRPC messages to reduce per-message overhead (while
   keeping Protobuf serialization).

## Conclusion

Apache Arrow Flight solves a real problem — high-throughput transfer of large
tabular datasets between analytics systems. Bonanza is not an analytics system.
Its data is graph-structured (DAGs of content-addressed objects), its access
patterns are point lookups and reference traversal, its messages are small and
heterogeneous, and its protocols require fine-grained bidirectional control
flow. Every one of these characteristics is an anti-pattern for Arrow Flight.

The existing gRPC + Protobuf stack is well-matched to Bonanza's requirements.
Effort would be better spent on optimizations within the current architecture
(vtprotobuf, lazy parsing, batching) than on adopting a fundamentally
mismatched data transfer framework.
