//go:build race

package scrub_test

// raceEnabled is the external-test-package twin of the in-package constant: the
// external tests (the ledger end-to-end chain and the ingestion harness) are
// correctness gates under -race and throughput gates on the plain build.
const raceEnabled = true
