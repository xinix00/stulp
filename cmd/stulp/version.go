package main

// version is what Stulp reports to Manage, MCP and attached apps. A release
// build may replace it with -ldflags "-X main.version=..."; a plain source
// build still tells the truth about the version it came from.
var version = "v0.8.5"
