// Package version holds the single source of truth for the aed version.
// CLI, MCP, and Laya client handshakes all read it so they cannot drift.
package version

// Version is the current release.
const Version = "0.4.0"
