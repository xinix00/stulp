package store

// files.go — where the one document's bytes come from and go to.
//
// The store knows what the document IS; it does not know what it lives on. On a
// host that is a filesystem, with the temp-file dance that keeps a crash from
// leaving half a document behind (document_os.go). On a HopOS node there is no
// filesystem in the process: an app asks the kernel, over an RPC, and the
// kernel owns the volume. Same two calls either way.
//
// The seam is here rather than a build tag inside loadDocument, because the
// platform is not the only reason to swap it: a test can hold the document in
// memory, and a backup can read one out of a copy.

// FileStore is the byte-level home of the document. ReadFile must report a
// missing file in a way that errors.Is(err, os.ErrNotExist) recognises — that is
// how a first run is told apart from a broken one.
type FileStore interface {
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte) error
}

// files is what the store uses. The default is set per platform by init in
// document_os.go or document_rpc.go.
var files FileStore

// UseFileStore replaces the backend. Call it before Open; the store reads the
// document there and then.
//
// A HopOS app calls this with its applib App, which is the only thing on a node
// that can reach a volume.
func UseFileStore(fs FileStore) { files = fs }
