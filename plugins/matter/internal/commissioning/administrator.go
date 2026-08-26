package commissioning

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"time"

	mattercrypto "github.com/xinix00/stulp/plugins/matter/internal/crypto"
	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

const (
	ClusterAdministratorCommissioning uint32 = 0x003C
	CommandOpenCommissioningWindow    uint32 = 0x00
	CommandRevokeCommissioning        uint32 = 0x02

	defaultWindowIterations = 10000
	timedInvokeTimeoutMS    = 10000
)

var forbiddenSetupPasscodes = map[uint32]bool{
	0: true, 11111111: true, 22222222: true, 33333333: true, 44444444: true,
	55555555: true, 66666666: true, 77777777: true, 88888888: true,
	99999999: true, 12345678: true, 87654321: true,
}

// WindowParameters contains both what is sent to the commissioned node and
// the short-lived secret shown to the user. Salt and Verifier must be treated
// as credentials for as long as the window remains open.
type WindowParameters struct {
	Timeout       uint16
	Passcode      uint32
	Discriminator uint16
	Iterations    uint32
	Salt          []byte
	Verifier      []byte
}

// NewWindowParameters creates an Enhanced Commissioning Method window. ECM
// gets a fresh passcode, discriminator and salt on every opening; it never
// exposes or reuses the accessory's factory setup code.
func NewWindowParameters(timeout time.Duration) (WindowParameters, error) {
	seconds := uint64(timeout / time.Second)
	if seconds == 0 || seconds > 0xFFFF {
		return WindowParameters{}, fmt.Errorf("commissioning window timeout must be 1..65535 seconds")
	}
	passcode, err := randomSetupPasscode()
	if err != nil {
		return WindowParameters{}, err
	}
	discriminator, err := cryptorand.Int(cryptorand.Reader, big.NewInt(1<<12))
	if err != nil {
		return WindowParameters{}, fmt.Errorf("generate Matter discriminator: %w", err)
	}
	salt := make([]byte, mattercrypto.MaxSaltBytes)
	if _, err := cryptorand.Read(salt); err != nil {
		return WindowParameters{}, fmt.Errorf("generate Matter PAKE salt: %w", err)
	}
	scalars, err := mattercrypto.DeriveScalars(passcode, salt, defaultWindowIterations)
	if err != nil {
		return WindowParameters{}, err
	}
	verifier, err := scalars.Register().Serialize()
	if err != nil {
		return WindowParameters{}, err
	}
	return WindowParameters{
		Timeout: uint16(seconds), Passcode: passcode, Discriminator: uint16(discriminator.Uint64()),
		Iterations: defaultWindowIterations, Salt: salt, Verifier: verifier,
	}, nil
}

func randomSetupPasscode() (uint32, error) {
	for {
		// Valid setup passcodes are 1..99999998 inclusive. Rejection of the
		// handful of memorable forbidden values is effectively constant time.
		value, err := cryptorand.Int(cryptorand.Reader, big.NewInt(99999998))
		if err != nil {
			return 0, fmt.Errorf("generate Matter setup passcode: %w", err)
		}
		passcode := uint32(value.Uint64()) + 1
		if !forbiddenSetupPasscodes[passcode] {
			return passcode, nil
		}
	}
}

func openWindowCommand(endpoint uint16, parameters WindowParameters) (im.Command, error) {
	if parameters.Timeout == 0 {
		return im.Command{}, errors.New("commissioning timeout is zero")
	}
	if parameters.Discriminator > 0x0FFF {
		return im.Command{}, errors.New("commissioning discriminator exceeds 12 bits")
	}
	if parameters.Iterations < mattercrypto.MinIterations || parameters.Iterations > mattercrypto.MaxIterations {
		return im.Command{}, errors.New("commissioning iteration count is outside Matter bounds")
	}
	if len(parameters.Salt) < mattercrypto.MinSaltBytes || len(parameters.Salt) > mattercrypto.MaxSaltBytes {
		return im.Command{}, errors.New("commissioning salt length is outside Matter bounds")
	}
	if len(parameters.Verifier) != 97 {
		return im.Command{}, fmt.Errorf("commissioning verifier is %d bytes, want 97", len(parameters.Verifier))
	}
	return im.Command{
		Path: im.CommandPath{Endpoint: endpoint, Cluster: ClusterAdministratorCommissioning, Command: CommandOpenCommissioningWindow},
		Fields: func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutUintWidth(tlv.Context(0), uint64(parameters.Timeout), 2)
			writer.PutBytes(tlv.Context(1), parameters.Verifier)
			writer.PutUintWidth(tlv.Context(2), uint64(parameters.Discriminator), 2)
			writer.PutUintWidth(tlv.Context(3), uint64(parameters.Iterations), 4)
			writer.PutBytes(tlv.Context(4), parameters.Salt)
			writer.EndContainer()
		},
	}, nil
}

// OpenCommissioningWindow gives another Matter administrator a bounded chance
// to commission the same node onto its own fabric. The command is a timed
// invoke as required by the Administrator Commissioning cluster.
func (c Client) OpenCommissioningWindow(ctx context.Context, parameters WindowParameters) error {
	command, err := openWindowCommand(c.Endpoint, parameters)
	if err != nil {
		return err
	}
	return c.invokeTimedStatus(ctx, command)
}

// RevokeCommissioning closes an open enhanced or basic window immediately.
func (c Client) RevokeCommissioning(ctx context.Context) error {
	return c.invokeTimedStatus(ctx, im.Command{
		Path: im.CommandPath{Endpoint: c.Endpoint, Cluster: ClusterAdministratorCommissioning, Command: CommandRevokeCommissioning},
	})
}

func (c Client) invokeTimedStatus(ctx context.Context, command im.Command) error {
	results, err := c.IM.InvokeTimed(ctx, timedInvokeTimeoutMS, command)
	if err != nil {
		return err
	}
	if len(results) != 1 {
		return fmt.Errorf("command 0x%02x returned %d results", command.Path.Command, len(results))
	}
	result := results[0]
	if !result.Status.OK() {
		return result.Status
	}
	if result.Path != command.Path {
		return fmt.Errorf("unexpected command response path %d/0x%04x/0x%02x",
			result.Path.Endpoint, result.Path.Cluster, result.Path.Command)
	}
	return nil
}
