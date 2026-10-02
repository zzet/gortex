// Gortex extension for the Pi coding agent (earendil-works/pi).
//
// Pi has no MCP support by design — the extension API's registerTool
// covers it. This extension does two things:
//
//   1. Exposes Gortex's graph tools as native Pi tools through a
//      persistent MCP stdio bridge: one `gortex mcp` child per session,
//      spoken to over standard JSON-RPC 2.0. The eager tools/list
//      surface is registered up front; the deferred catalogue is
//      reached via the server's own `tools_search` tool, whose
//      promotions are re-synced into Pi's registry on
//      notifications/tools/list_changed.
//
// Truncated here: the fixture carries the header the ownership test
// keys on, not the whole extension.

export default {};
