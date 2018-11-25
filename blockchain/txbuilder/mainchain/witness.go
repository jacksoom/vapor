package mainchain

import (
	"context"

	"github.com/vapor/blockchain/txbuilder"
	"github.com/vapor/crypto/ed25519/chainkd"
	chainjson "github.com/vapor/encoding/json"
	"github.com/vapor/errors"
)

// SignFunc is the function passed into Sign that produces
// a signature for a given xpub, derivation path, and hash.
type SignFunc func(context.Context, chainkd.XPub, [][]byte, [32]byte, string) ([]byte, error)

// MaterializeWitnesses takes a filled in Template and "materializes"
// each witness component, turning it into a vector of arguments for
// the tx's input witness, creating a fully-signed transaction.
func materializeWitnesses(txTemplate *Template) error {
	msg := txTemplate.Transaction

	if msg == nil {
		return errors.Wrap(txbuilder.ErrMissingRawTx)
	}

	if len(txTemplate.SigningInstructions) > len(msg.Inputs) {
		return errors.Wrap(txbuilder.ErrBadInstructionCount)
	}

	for i, sigInst := range txTemplate.SigningInstructions {
		if msg.Inputs[sigInst.Position] == nil {
			return errors.WithDetailf(txbuilder.ErrBadTxInputIdx, "signing instruction %d references missing tx input %d", i, sigInst.Position)
		}

		var witness [][]byte
		for j, wc := range sigInst.WitnessComponents {
			err := wc.Materialize(&witness)
			if err != nil {
				return errors.WithDetailf(err, "error in witness component %d of input %d", j, i)
			}
		}
		msg.SetInputArguments(sigInst.Position, witness)
	}

	return nil
}

func signedCount(signs []chainjson.HexBytes) (count int) {
	for _, sign := range signs {
		if len(sign) > 0 {
			count++
		}
	}
	return
}

// SignProgress check is all the sign requirement are satisfy
func SignProgress(txTemplate *Template) bool {
	for _, sigInst := range txTemplate.SigningInstructions {
		for _, wc := range sigInst.WitnessComponents {
			switch sw := wc.(type) {
			case *SignatureWitness:
				if signedCount(sw.Sigs) < sw.Quorum {
					return false
				}
			case *RawTxSigWitness:
				if signedCount(sw.Sigs) < sw.Quorum {
					return false
				}
			}
		}
	}
	return true
}
