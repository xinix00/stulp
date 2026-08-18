//go:build tamago

package store

// document_rpc.go — on a node there is no filesystem in the process.
//
// A HopOS app reaches a volume through the kernel, so the backend has to be
// injected by whoever holds that connection (the app's applib handle). Until it
// is, every read and write says so instead of quietly working on nothing: a
// controller that starts with an empty document because its storage was never
// wired would look exactly like a first run, and then write that emptiness back
// over the real thing.

import "errors"

func init() { files = noFiles{} }

type noFiles struct{}

var errNoFileStore = errors.New("store: no file store on this platform -- call store.UseFileStore first (a HopOS app passes its applib App)")

func (noFiles) ReadFile(string) ([]byte, error) { return nil, errNoFileStore }
func (noFiles) WriteFile(string, []byte) error  { return errNoFileStore }
