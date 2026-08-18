//go:build !tamago

package webapi

// Host backups can contain complete app bundles. The archive is held while its
// central directory is validated; the expanded archive has a separate 2 GiB
// safety limit in package backup.
const maxRestoreUploadBytes int64 = 512 << 20
