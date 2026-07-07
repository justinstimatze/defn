// Package projection implements defn's projection-level edit vocabulary:
// small, mechanically-verifiable source-code edit primitives whose `put`
// side (edit application) satisfies a byte-exact or quotient-lens PUTGET
// contract against the `get` side (projection read).
//
// Each operator is a pure function over a definition body string. The
// wiring into the MCP layer lives in internal/mcp/server.go; the pure
// functions live here so their PUTGET goldens can be tested without any
// DB or MCP dependencies.
//
// See project_putget_edit_vocab_design and project_projection_phase_c_next
// memory for the design contract and phase plan.
package projection
