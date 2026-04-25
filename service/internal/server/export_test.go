// export_test.go — exposes unexported server symbols for external (_test) tests.
// This file is compiled only during `go test` (it has the _test.go suffix).
package server

// CompressTranscript exposes the unexported compressTranscript function so that
// tests in the server_test package can call it without promoting the function to
// the public API.
var CompressTranscript = compressTranscript

// TranscriptCompressionEnabled is a pointer to the package-level flag so tests
// can enable compression to exercise the algorithm while production keeps it off.
var TranscriptCompressionEnabled = &transcriptCompressionEnabled
