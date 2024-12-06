package changeset_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"

	"math"
	"testing"

	"github.com/smartcontractkit/ccip-owner-contracts/pkg/gethwrappers"
	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"
	"github.com/smartcontractkit/chainlink-protos/job-distributor/v1/node"
	"github.com/smartcontractkit/chainlink/deployment"
	commonchangeset "github.com/smartcontractkit/chainlink/deployment/common/changeset"
	commontypes "github.com/smartcontractkit/chainlink/deployment/common/types"
	"github.com/smartcontractkit/chainlink/deployment/environment/memory"
	"github.com/smartcontractkit/chainlink/deployment/keystone"
	kslib "github.com/smartcontractkit/chainlink/deployment/keystone"
	kschangeset "github.com/smartcontractkit/chainlink/deployment/keystone/changeset"
	kcr "github.com/smartcontractkit/chainlink/v2/core/gethwrappers/keystone/generated/capabilities_registry"
	"github.com/smartcontractkit/chainlink/v2/core/services/keystore/keys/p2pkey"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
	"golang.org/x/exp/maps"
)

func TestSetupTestEnv(t *testing.T) {
	t.Parallel()
	ctx := tests.Context(t)
	for _, useMCMS := range []bool{true, false} {
		te := SetupTestEnv(t, TestConfig{
			WFDonConfig:     DonConfig{N: 4},
			AssetDonConfig:  DonConfig{N: 4},
			WriterDonConfig: DonConfig{N: 4},
			NumChains:       3,
			UseMCMS:         useMCMS,
		})
		t.Run(fmt.Sprintf("set up test env using MCMS: %T", useMCMS), func(t *testing.T) {
			require.NotNil(t, te.Env.ExistingAddresses)
			require.Len(t, te.Env.Chains, 3)
			require.NotEmpty(t, te.RegistrySelector)
			require.NotNil(t, te.Env.Offchain)
			r, err := te.Env.Offchain.ListNodes(ctx, &node.ListNodesRequest{})
			require.NoError(t, err)
			require.Len(t, r.Nodes, 12)
		})
	}
}

type DonConfig struct {
	N int
}

func (c DonConfig) Validate() error {
	if c.N < 4 {
		return errors.New("N must be at least 4")
	}
	return nil
}

// TODO: separate the config into different types; wf should expand to types of ocr keybundles; writer to target chains; ...
type WFDonConfig = DonConfig
type AssetDonConfig = DonConfig
type WriterDonConfig = DonConfig

type TestConfig struct {
	WFDonConfig
	AssetDonConfig
	WriterDonConfig
	NumChains int

	UseMCMS bool
}

func (c TestConfig) Validate() error {
	if err := c.WFDonConfig.Validate(); err != nil {
		return err
	}
	if err := c.AssetDonConfig.Validate(); err != nil {
		return err
	}
	if err := c.WriterDonConfig.Validate(); err != nil {
		return err
	}
	if c.NumChains < 1 {
		return errors.New("NumChains must be at least 1")
	}
	return nil
}

type TestEnv struct {
	t                *testing.T
	Env              deployment.Environment
	RegistrySelector uint64

	WFNodes    map[string]memory.Node
	CWNodes    map[string]memory.Node
	AssetNodes map[string]memory.Node
}

func (te TestEnv) ContractSets() map[uint64]kslib.ContractSet {
	r, err := kslib.GetContractSets(te.Env.Logger, &kslib.GetContractSetsRequest{
		Chains:      te.Env.Chains,
		AddressBook: te.Env.ExistingAddresses,
	})
	require.NoError(te.t, err)
	return r.ContractSets
}

// SetupTestEnv sets up a keystone test environment with the given configuration
func SetupTestEnv(t *testing.T, c TestConfig) TestEnv {
	require.NoError(t, c.Validate())
	lggr := logger.Test(t)
	ctx := tests.Context(t)
	chains, _ := memory.NewMemoryChains(t, c.NumChains, 1)
	registryChainSel := registryChain(t, chains)
	// note that all the nodes require TOML configuration of the cap registry address
	// and writers need forwarder address as TOML config
	// we choose to use changesets to deploy the initial contracts because that's how it's done in the real world
	// this requires a initial environment to house the address book
	e := deployment.Environment{
		Logger:            lggr,
		Chains:            chains,
		ExistingAddresses: deployment.NewMemoryAddressBook(),
	}
	e, err := commonchangeset.ApplyChangesets(t, e, nil, []commonchangeset.ChangesetApplication{
		{
			Changeset: commonchangeset.WrapChangeSet(kschangeset.DeployCapabilityRegistry),
			Config:    registryChainSel,
		},
		{
			Changeset: commonchangeset.WrapChangeSet(kschangeset.DeployOCR3),
			Config:    registryChainSel,
		},
		{
			Changeset: commonchangeset.WrapChangeSet(kschangeset.DeployForwarder),
			Config:    registryChainSel,
		},
	})
	require.NoError(t, err)
	require.NotNil(t, e)
	require.Len(t, e.Chains, c.NumChains)
	validateInitialChainState(t, e, registryChainSel)
	// now that we have the initial contracts deployed, we can configure the nodes with the addresses
	// TODO: configure the nodes with the correct override functions
	crConfig := deployment.CapabilityRegistryConfig{
		EVMChainID: registryChainSel,
		Contract:   [20]byte{},
	}

	wfChains := map[uint64]deployment.Chain{}
	wfChains[registryChainSel] = chains[registryChainSel]
	wfNodes := memory.NewNodes(t, zapcore.InfoLevel, wfChains, c.WFDonConfig.N, 0, crConfig)
	require.Len(t, wfNodes, c.WFDonConfig.N)

	writerChains := map[uint64]deployment.Chain{}
	maps.Copy(writerChains, chains)
	cwNodes := memory.NewNodes(t, zapcore.InfoLevel, writerChains, c.WriterDonConfig.N, 0, crConfig)
	require.Len(t, cwNodes, c.WriterDonConfig.N)

	assetChains := map[uint64]deployment.Chain{}
	assetChains[registryChainSel] = chains[registryChainSel]
	assetNodes := memory.NewNodes(t, zapcore.InfoLevel, assetChains, c.AssetDonConfig.N, 0, crConfig)
	require.Len(t, assetNodes, c.AssetDonConfig.N)

	// TODO: partition nodes into multiple nops

	wfDon := keystone.DonCapabilities{
		Name: keystone.WFDonName,
		Nops: []keystone.NOP{
			{
				Name:  "nop 1",
				Nodes: maps.Keys(wfNodes),
			},
		},
		Capabilities: []kcr.CapabilitiesRegistryCapability{keystone.OCR3Cap},
	}
	cwDon := keystone.DonCapabilities{
		Name: keystone.TargetDonName,
		Nops: []keystone.NOP{
			{
				Name:  "nop 2",
				Nodes: maps.Keys(cwNodes),
			},
		},
		Capabilities: []kcr.CapabilitiesRegistryCapability{keystone.WriteChainCap},
	}
	assetDon := keystone.DonCapabilities{
		Name: keystone.StreamDonName,
		Nops: []keystone.NOP{
			{
				Name:  "nop 3",
				Nodes: maps.Keys(assetNodes),
			},
		},
		Capabilities: []kcr.CapabilitiesRegistryCapability{keystone.StreamTriggerCap},
	}

	allChains := make(map[uint64]deployment.Chain)
	maps.Copy(allChains, chains)

	allNodes := make(map[string]memory.Node)
	maps.Copy(allNodes, wfNodes)
	maps.Copy(allNodes, cwNodes)
	maps.Copy(allNodes, assetNodes)
	env := memory.NewMemoryEnvironmentFromChainsNodes(func() context.Context { return ctx }, lggr, allChains, allNodes)
	// set the env addresses to the deployed addresses that were created prior to configuring the nodes
	err = env.ExistingAddresses.Merge(e.ExistingAddresses)
	require.NoError(t, err)

	var ocr3Config = keystone.OracleConfigWithSecrets{
		OracleConfig: keystone.OracleConfig{
			MaxFaultyOracles: len(wfNodes) / 3,
		},
		OCRSecrets: deployment.XXXGenerateTestOCRSecrets(),
	}
	var allDons = []keystone.DonCapabilities{wfDon, cwDon, assetDon}

	_, err = kschangeset.ConfigureInitialContractsChangeset(env, kschangeset.InitialContractsCfg{
		RegistryChainSel: registryChainSel,
		Dons:             allDons,
		OCR3Config:       &ocr3Config,
	})
	require.NoError(t, err)
	// TODO: KS-rm_deploy_opt
	//require.Nil(t, csOut.AddressBook, "no new addresses should be created in configure initial contracts")
	//require.NoError(t, env.ExistingAddresses.Merge(csOut.AddressBook))

	req := &keystone.GetContractSetsRequest{
		Chains:      env.Chains,
		AddressBook: env.ExistingAddresses,
	}

	contractSetsResp, err := keystone.GetContractSets(lggr, req)
	require.NoError(t, err)
	require.Len(t, contractSetsResp.ContractSets, len(env.Chains))
	// check the registry
	gotRegistry := contractSetsResp.ContractSets[registryChainSel].CapabilitiesRegistry
	require.NotNil(t, gotRegistry)
	// validate the registry
	// check the nodes
	gotNodes, err := gotRegistry.GetNodes(nil)
	require.NoError(t, err)
	require.Len(t, gotNodes, len(allNodes))
	validateNodes(t, gotRegistry, wfNodes, expectedHashedCapabilities(t, gotRegistry, wfDon))
	validateNodes(t, gotRegistry, cwNodes, expectedHashedCapabilities(t, gotRegistry, cwDon))
	validateNodes(t, gotRegistry, assetNodes, expectedHashedCapabilities(t, gotRegistry, assetDon))

	// check the dons
	validateDon(t, gotRegistry, wfNodes, wfDon)
	validateDon(t, gotRegistry, cwNodes, cwDon)
	validateDon(t, gotRegistry, assetNodes, assetDon)

	if c.UseMCMS {
		// TODO: mcms on all the chains, currently only on the registry chain. need to fix this for forwarders
		// deploy, configure and xfer ownership of MCMS
		timelockCfgs := make(map[uint64]commontypes.MCMSWithTimelockConfig)
		for sel := range env.Chains {
			t.Logf("Enabling MCMS on chain %d", sel)
			timelockCfgs[sel] = commontypes.MCMSWithTimelockConfig{
				Canceller:         commonchangeset.SingleGroupMCMS(t),
				Bypasser:          commonchangeset.SingleGroupMCMS(t),
				Proposer:          commonchangeset.SingleGroupMCMS(t),
				TimelockExecutors: env.AllDeployerKeys(),
				TimelockMinDelay:  big.NewInt(0),
			}
		}
		env, err = commonchangeset.ApplyChangesets(t, env, nil, []commonchangeset.ChangesetApplication{
			{
				Changeset: commonchangeset.WrapChangeSet(commonchangeset.DeployMCMSWithTimelock),
				Config:    timelockCfgs,
			},
		})
		require.NoError(t, err)
		// extract the MCMS address
		r, err := kslib.GetContractSets(lggr, &kslib.GetContractSetsRequest{
			Chains:      env.Chains,
			AddressBook: env.ExistingAddresses,
		})
		require.NoError(t, err)
		for sel := range env.Chains {
			mcms := r.ContractSets[sel].MCMSWithTimelockState
			require.NotNil(t, mcms, "MCMS not found on chain %d", sel)
			require.NoError(t, mcms.Validate())

			// transfer ownership of all contracts to the MCMS
			env, err = commonchangeset.ApplyChangesets(t, env, map[uint64]*gethwrappers.RBACTimelock{sel: mcms.Timelock}, []commonchangeset.ChangesetApplication{
				{
					Changeset: commonchangeset.WrapChangeSet(kschangeset.AcceptAllOwnershipsProposal),
					Config: &kschangeset.AcceptAllOwnershipRequest{
						ChainSelector: sel,
						MinDelay:      0,
					},
				},
			})
			require.NoError(t, err)
		}
	}
	return TestEnv{
		t:                t,
		Env:              env,
		RegistrySelector: registryChainSel,
		WFNodes:          wfNodes,
		CWNodes:          cwNodes,
		AssetNodes:       assetNodes,
	}
}

func registryChain(t *testing.T, chains map[uint64]deployment.Chain) uint64 {
	var registryChainSel uint64 = math.MaxUint64
	for sel := range chains {
		if sel < registryChainSel {
			registryChainSel = sel
		}
	}
	return registryChainSel
}

// validateInitialChainState checks that the initial chain state
// has the expected contracts deployed
func validateInitialChainState(t *testing.T, env deployment.Environment, registryChainSel uint64) {
	ad := env.ExistingAddresses
	// all contracts on registry chain
	registryChainAddrs, err := ad.AddressesForChain(registryChainSel)
	require.NoError(t, err)
	require.Len(t, registryChainAddrs, 3) // registry, ocr3, forwarder
	// only forwarder on non-home chain
	for sel := range env.Chains {
		chainAddrs, err := ad.AddressesForChain(sel)
		require.NoError(t, err)
		if sel != registryChainSel {
			require.Len(t, chainAddrs, 1)
		} else {
			require.Len(t, chainAddrs, 3)
		}
		containsForwarder := false
		for _, tv := range chainAddrs {
			if tv.Type == keystone.KeystoneForwarder {
				containsForwarder = true
				break
			}
		}
		require.True(t, containsForwarder, "no forwarder found in %v on chain %d for target don", chainAddrs, sel)
	}
}

// validateNodes checks that the nodes exist and have the expected capabilities
func validateNodes(t *testing.T, gotRegistry *kcr.CapabilitiesRegistry, nodes map[string]memory.Node, expectedHashedCaps [][32]byte) {
	gotNodes, err := gotRegistry.GetNodesByP2PIds(nil, p2pIDs(t, maps.Keys(nodes)))
	require.NoError(t, err)
	require.Len(t, gotNodes, len(nodes))
	for _, n := range gotNodes {
		require.Equal(t, expectedHashedCaps, n.HashedCapabilityIds)
	}
}

// validateDon checks that the don exists and has the expected capabilities
func validateDon(t *testing.T, gotRegistry *kcr.CapabilitiesRegistry, nodes map[string]memory.Node, don kslib.DonCapabilities) {
	gotDons, err := gotRegistry.GetDONs(nil)
	require.NoError(t, err)
	wantP2PID := sortedHash(p2pIDs(t, maps.Keys(nodes)))
	found := false
	for _, have := range gotDons {
		gotP2PID := sortedHash(have.NodeP2PIds)
		if gotP2PID == wantP2PID {
			found = true
			gotCapIDs := capIDs(t, have.CapabilityConfigurations)
			require.Equal(t, expectedHashedCapabilities(t, gotRegistry, don), gotCapIDs)
			break
		}
	}
	require.True(t, found, "don not found in registry")
}

func capIDs(t *testing.T, cfgs []kcr.CapabilitiesRegistryCapabilityConfiguration) [][32]byte {
	var out [][32]byte
	for _, cfg := range cfgs {
		out = append(out, cfg.CapabilityId)
	}
	return out
}

func ptr[T any](t T) *T {
	return &t
}

func p2pIDs(t *testing.T, vals []string) [][32]byte {
	var out [][32]byte
	for _, v := range vals {
		id, err := p2pkey.MakePeerID(v)
		require.NoError(t, err)
		out = append(out, id)
	}
	return out
}

func expectedHashedCapabilities(t *testing.T, registry *kcr.CapabilitiesRegistry, don kslib.DonCapabilities) [][32]byte {
	out := make([][32]byte, len(don.Capabilities))
	var err error
	for i, cap := range don.Capabilities {
		out[i], err = registry.GetHashedCapabilityId(nil, cap.LabelledName, cap.Version)
		require.NoError(t, err)
	}
	return out
}

func sortedHash(p2pids [][32]byte) string {
	sha256Hash := sha256.New()
	sort.Slice(p2pids, func(i, j int) bool {
		return bytes.Compare(p2pids[i][:], p2pids[j][:]) < 0
	})
	for _, id := range p2pids {
		sha256Hash.Write(id[:])
	}
	return hex.EncodeToString(sha256Hash.Sum(nil))
}
