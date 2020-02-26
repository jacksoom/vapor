package database

import (
	"os"
	"testing"

	dbm "github.com/bytom/vapor/database/leveldb"
	"github.com/bytom/vapor/database/storage"
	"github.com/bytom/vapor/protocol/bc"
	"github.com/bytom/vapor/protocol/state"
	"github.com/bytom/vapor/testutil"
)

func TestSaveUtxoView(t *testing.T) {
	testDB := dbm.NewDB("testdb", "leveldb", "temp")
	batch := testDB.NewBatch()
	defer func() {
		testDB.Close()
		os.RemoveAll("temp")
	}()

	cases := []struct {
		hash      bc.Hash
		utxoEntry *storage.UtxoEntry
		exist     bool
	}{
		{
			hash:      bc.Hash{V0: 0},
			utxoEntry: storage.NewUtxoEntry(storage.CoinbaseUTXOType, 0, true),
			exist:     true,
		},
		{
			hash:      bc.Hash{V0: 1},
			utxoEntry: storage.NewUtxoEntry(storage.CoinbaseUTXOType, 0, false),
			exist:     true,
		},
		{
			hash:      bc.Hash{V0: 2},
			utxoEntry: storage.NewUtxoEntry(storage.NormalUTXOType, 0, false),
			exist:     true,
		},
		{
			hash:      bc.Hash{V0: 3},
			utxoEntry: storage.NewUtxoEntry(storage.NormalUTXOType, 0, true),
			exist:     false,
		},
		{
			hash:      bc.Hash{V0: 4},
			utxoEntry: storage.NewUtxoEntry(storage.CrosschainUTXOType, 0, true),
			exist:     true,
		},
		{
			hash:      bc.Hash{V0: 5},
			utxoEntry: storage.NewUtxoEntry(storage.CrosschainUTXOType, 0, false),
			exist:     false,
		},
		{
			hash:      bc.Hash{V0: 6},
			utxoEntry: storage.NewUtxoEntry(storage.VoteUTXOType, 0, true),
			exist:     true,
		},
		{
			hash:      bc.Hash{V0: 7},
			utxoEntry: storage.NewUtxoEntry(storage.VoteUTXOType, 0, false),
			exist:     true,
		},
	}

	view := state.NewUtxoViewpoint()
	for _, c := range cases {
		view.Entries[c.hash] = c.utxoEntry
	}

	saveUtxoView(batch, view)
	batch.Write()

	for _, c := range cases {
		entry, err := getUtxo(testDB, &c.hash)

		if !c.exist {
			if err == nil {
				t.Errorf("%v should be unexisted, but it's in the db", c)
			}
			continue
		}

		if !testutil.DeepEqual(entry, c.utxoEntry) {
			t.Errorf("%v utxo in the db isn't match", c)
		}
	}
}

func TestGetTransactionsUtxo(t *testing.T) {
	testDB := dbm.NewDB("testdb", "leveldb", "temp")
	defer func() {
		testDB.Close()
		os.RemoveAll("temp")
	}()

	batch := testDB.NewBatch()
	inputView := state.NewUtxoViewpoint()
	for i := 0; i <= 2; i++ {
		inputView.Entries[bc.Hash{V0: uint64(i)}] = storage.NewUtxoEntry(storage.NormalUTXOType, uint64(i), false)
	}
	inputView.Entries[bc.Hash{V0: uint64(3)}] = storage.NewUtxoEntry(storage.CrosschainUTXOType, uint64(3), true)
	inputView.Entries[bc.Hash{V0: uint64(4)}] = storage.NewUtxoEntry(storage.CoinbaseUTXOType, uint64(4), false)
	inputView.Entries[bc.Hash{V0: uint64(5)}] = storage.NewUtxoEntry(storage.VoteUTXOType, uint64(5), false)

	saveUtxoView(batch, inputView)
	batch.Write()

	cases := []struct {
		txs       []*bc.Tx
		inputView *state.UtxoViewpoint
		fetchView *state.UtxoViewpoint
		err       bool
	}{

		{
			txs: []*bc.Tx{
				&bc.Tx{
					SpentOutputIDs: []bc.Hash{bc.Hash{V0: 10}},
				},
			},
			inputView: state.NewUtxoViewpoint(),
			fetchView: state.NewUtxoViewpoint(),
			err:       false,
		},
		{
			txs: []*bc.Tx{
				&bc.Tx{
					MainchainOutputIDs: []bc.Hash{
						bc.Hash{V0: 10},
						bc.Hash{V0: 3},
					},
				},
			},
			inputView: state.NewUtxoViewpoint(),
			fetchView: &state.UtxoViewpoint{
				Entries: map[bc.Hash]*storage.UtxoEntry{
					bc.Hash{V0: 10}: storage.NewUtxoEntry(storage.CrosschainUTXOType, 0, false),
					bc.Hash{V0: 3}:  storage.NewUtxoEntry(storage.CrosschainUTXOType, 3, true),
				},
			},
			err: false,
		},
		{
			txs: []*bc.Tx{
				&bc.Tx{
					SpentOutputIDs: []bc.Hash{
						bc.Hash{V0: 4},
						bc.Hash{V0: 5},
						bc.Hash{V0: 6}, //no spentOutputID store
					},
				},
			},
			inputView: state.NewUtxoViewpoint(),
			fetchView: &state.UtxoViewpoint{
				Entries: map[bc.Hash]*storage.UtxoEntry{
					bc.Hash{V0: 4}: storage.NewUtxoEntry(storage.CoinbaseUTXOType, 4, false),
					bc.Hash{V0: 5}: storage.NewUtxoEntry(storage.VoteUTXOType, 5, false),
				},
			},
			err: false,
		},
		{
			txs: []*bc.Tx{
				&bc.Tx{
					SpentOutputIDs: []bc.Hash{bc.Hash{V0: 0}},
				},
			},
			inputView: state.NewUtxoViewpoint(),
			fetchView: &state.UtxoViewpoint{
				Entries: map[bc.Hash]*storage.UtxoEntry{
					bc.Hash{V0: 0}: storage.NewUtxoEntry(storage.NormalUTXOType, 0, false),
				},
			},
			err: false,
		},
		{
			txs: []*bc.Tx{
				&bc.Tx{
					SpentOutputIDs: []bc.Hash{
						bc.Hash{V0: 0},
						bc.Hash{V0: 1},
					},
				},
			},
			inputView: state.NewUtxoViewpoint(),
			fetchView: &state.UtxoViewpoint{
				Entries: map[bc.Hash]*storage.UtxoEntry{
					bc.Hash{V0: 0}: storage.NewUtxoEntry(storage.NormalUTXOType, 0, false),
					bc.Hash{V0: 1}: storage.NewUtxoEntry(storage.NormalUTXOType, 1, false),
				},
			},
			err: false,
		},
		{
			txs: []*bc.Tx{
				&bc.Tx{
					SpentOutputIDs: []bc.Hash{
						bc.Hash{V0: 0},
						bc.Hash{V0: 1},
					},
				},
				&bc.Tx{
					SpentOutputIDs: []bc.Hash{
						bc.Hash{V0: 2},
					},
				},
			},
			inputView: state.NewUtxoViewpoint(),
			fetchView: &state.UtxoViewpoint{
				Entries: map[bc.Hash]*storage.UtxoEntry{
					bc.Hash{V0: 0}: storage.NewUtxoEntry(storage.NormalUTXOType, 0, false),
					bc.Hash{V0: 1}: storage.NewUtxoEntry(storage.NormalUTXOType, 1, false),
					bc.Hash{V0: 2}: storage.NewUtxoEntry(storage.NormalUTXOType, 2, false),
				},
			},
			err: false,
		},
		{
			txs: []*bc.Tx{
				&bc.Tx{
					SpentOutputIDs: []bc.Hash{bc.Hash{V0: 0}},
				},
			},
			inputView: &state.UtxoViewpoint{
				Entries: map[bc.Hash]*storage.UtxoEntry{
					bc.Hash{V0: 0}: storage.NewUtxoEntry(storage.NormalUTXOType, 1, false),
				},
			},
			fetchView: &state.UtxoViewpoint{
				Entries: map[bc.Hash]*storage.UtxoEntry{
					bc.Hash{V0: 0}: storage.NewUtxoEntry(storage.NormalUTXOType, 1, false),
				},
			},
			err: false,
		},
	}

	for i, c := range cases {
		if err := getTransactionsUtxo(testDB, c.inputView, c.txs); c.err != (err != nil) {
			t.Errorf("test case %d, want err = %v, get err = %v", i, c.err, err)
		}
		if !testutil.DeepEqual(c.inputView, c.fetchView) {
			t.Errorf("test case %d, want %v, get %v", i, c.fetchView, c.inputView)
		}
	}
}
