//go:build !linux && !darwin

package appproto

import (
	"errors"
	"net"
	"os"
)

// Op een platform zonder unix-sockets bestaat er geen socketpair, en dus ook
// geen app die Stulp zelf start: starten is fork+exec, en dat is precies wat
// HopOS niet heeft (een app is daar een slot-image dat de node plaatst).
//
// Het type blijft bestaan zodat de aanroeper hetzelfde leest op elk doel; wat
// verschilt is dat NewPair hier luid weigert in plaats van een halve
// verbinding op te leveren. De weg die het wél doet staat in de melding.
type Pair struct {
	Parent *os.File
	Child  *os.File
}

var errNoSocketpair = errors.New("attach: this platform has no socketpair, so stulp cannot start an app itself -- let the app start first and announce itself (--attach-port)")

// NewPair weigert: zie errNoSocketpair.
func NewPair() (*Pair, error) { return nil, errNoSocketpair }

// Conn weigert om dezelfde reden; een Pair kan hier niet bestaan.
func (p *Pair) Conn() (net.Conn, error) { return nil, errNoSocketpair }
