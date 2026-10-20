// Package modeltrace implements ModelTrace challenge generation, output parsing,
// and closed-set fingerprint scoring using immutable bank snapshots.
//
// It is a focused library, not a framework runtime unit. It performs no network,
// filesystem, environment, registration, background work, or credential access.
// Callers generate challenges, obtain model output through their own owner, and
// submit typed Request values to Engine.Score. Sampling provenance and PATH or
// QUALITY validation remain responsibilities of the integrating application.
package modeltrace
