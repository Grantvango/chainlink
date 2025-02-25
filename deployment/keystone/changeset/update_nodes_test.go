package changeset_test

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/maps"

	"github.com/smartcontractkit/ccip-owner-contracts/pkg/gethwrappers"
	commonchangeset "github.com/smartcontractkit/chainlink/deployment/common/changeset"
	"github.com/smartcontractkit/chainlink/deployment/keystone/changeset"
	"github.com/smartcontractkit/chainlink/v2/core/services/keystore/keys/p2pkey"
)

func TestUpdateNodes(t *testing.T) {
	t.Parallel()

	t.Run("no mcms", func(t *testing.T) {
		te := SetupTestEnv(t, TestConfig{
			WFDonConfig:     DonConfig{N: 4},
			AssetDonConfig:  DonConfig{N: 4},
			WriterDonConfig: DonConfig{N: 4},
			NumChains:       1,
		})

		updates := make(map[p2pkey.PeerID]changeset.NodeUpdate)
		i := uint8(0)
		for id, _ := range te.WFNodes {
			k, err := p2pkey.MakePeerID(id)
			require.NoError(t, err)
			pubKey := [32]byte{31: i + 1}
			// don't set capabilities or nop b/c those must already exist in the contract
			// those ops must be a different proposal when using MCMS
			updates[k] = changeset.NodeUpdate{
				EncryptionPublicKey: hex.EncodeToString(pubKey[:]),
				Signer:              [32]byte{0: i + 1},
			}
			i++
		}

		cfg := changeset.UpdateNodesRequest{
			RegistryChainSel: te.RegistrySelector,
			P2pToUpdates:     updates,
		}

		csOut, err := changeset.UpdateNodes(te.Env, &cfg)
		require.NoError(t, err)
		require.Len(t, csOut.Proposals, 0)
		require.Nil(t, csOut.AddressBook)

		validateUpdate(t, te, updates)
	})

	t.Run("with mcms", func(t *testing.T) {
		te := SetupTestEnv(t, TestConfig{
			WFDonConfig:     DonConfig{N: 4},
			AssetDonConfig:  DonConfig{N: 4},
			WriterDonConfig: DonConfig{N: 4},
			NumChains:       1,
			UseMCMS:         true,
		})

		updates := make(map[p2pkey.PeerID]changeset.NodeUpdate)
		i := uint8(0)
		for id, _ := range te.WFNodes {
			k, err := p2pkey.MakePeerID(id)
			require.NoError(t, err)
			pubKey := [32]byte{31: i + 1}
			// don't set capabilities or nop b/c those must already exist in the contract
			// those ops must be a different proposal when using MCMS
			updates[k] = changeset.NodeUpdate{
				EncryptionPublicKey: hex.EncodeToString(pubKey[:]),
				Signer:              [32]byte{0: i + 1},
			}
			i++
		}

		cfg := changeset.UpdateNodesRequest{
			RegistryChainSel: te.RegistrySelector,
			P2pToUpdates:     updates,
			UseMCMS:          true,
		}

		csOut, err := changeset.UpdateNodes(te.Env, &cfg)
		require.NoError(t, err)
		require.Len(t, csOut.Proposals, 1)
		require.Nil(t, csOut.AddressBook)

		// now apply the changeset such that the proposal is signed and execed
		contracts := te.ContractSets()[te.RegistrySelector]
		timelocks := map[uint64]*gethwrappers.RBACTimelock{
			te.RegistrySelector: contracts.Timelock,
		}
		_, err = commonchangeset.ApplyChangesets(t, te.Env, timelocks, []commonchangeset.ChangesetApplication{
			{
				Changeset: commonchangeset.WrapChangeSet(changeset.UpdateNodes),
				Config: &changeset.UpdateNodesRequest{
					RegistryChainSel: te.RegistrySelector,
					P2pToUpdates:     updates,
					UseMCMS:          true,
				},
			},
		})
		require.NoError(t, err)

		validateUpdate(t, te, updates)
	})

}

// validateUpdate checks reads nodes from the registry and checks they have the expected updates
func validateUpdate(t *testing.T, te TestEnv, expected map[p2pkey.PeerID]changeset.NodeUpdate) {
	registry := te.ContractSets()[te.RegistrySelector].CapabilitiesRegistry
	wfP2PIDs := p2pIDs(t, maps.Keys(te.WFNodes))
	nodes, err := registry.GetNodesByP2PIds(nil, wfP2PIDs)
	require.NoError(t, err)
	require.Len(t, nodes, len(wfP2PIDs))
	for _, node := range nodes {
		// only check the fields that were updated
		assert.Equal(t, expected[node.P2pId].EncryptionPublicKey, hex.EncodeToString(node.EncryptionPublicKey[:]))
		assert.Equal(t, expected[node.P2pId].Signer, node.Signer)
	}
}
