//go:build tamago

package webapi

// A HopOS Stulp only has announced/rootless apps, so its backup is essentially
// stulp.json plus a tiny manifest. Keep a malicious upload away from the slot's
// deliberately small memory budget.
const maxRestoreUploadBytes int64 = 8 << 20
