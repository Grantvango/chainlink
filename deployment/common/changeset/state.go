package changeset

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	owner_helpers "github.com/smartcontractkit/ccip-owner-contracts/pkg/gethwrappers"

	"github.com/smartcontractkit/chainlink/deployment"
	"github.com/smartcontractkit/chainlink/deployment/common/types"
	"github.com/smartcontractkit/chainlink/deployment/common/view/v1_0"
	"github.com/smartcontractkit/chainlink/v2/core/gethwrappers/generated/link_token_interface"
	"github.com/smartcontractkit/chainlink/v2/core/gethwrappers/shared/generated/link_token"
)

// MCMSWithTimelockState holds the Go bindings
// for a MCMSWithTimelock contract deployment.
// It is public for use in product specific packages.
// Either all fields are nil or all fields are non-nil.
type MCMSWithTimelockState struct {
	CancellerMcm *owner_helpers.ManyChainMultiSig
	BypasserMcm  *owner_helpers.ManyChainMultiSig
	ProposerMcm  *owner_helpers.ManyChainMultiSig
	Timelock     *owner_helpers.RBACTimelock
}

// Validate checks that all fields are non-nil, ensuring it's ready
// for use generating views or interactions.
func (state MCMSWithTimelockState) Validate() error {
	if state.Timelock == nil {
		return errors.New("timelock not found")
	}
	if state.CancellerMcm == nil {
		return errors.New("canceller not found")
	}
	if state.ProposerMcm == nil {
		return errors.New("proposer not found")
	}
	if state.BypasserMcm == nil {
		return errors.New("bypasser not found")
	}
	return nil
}

func (state MCMSWithTimelockState) GenerateMCMSWithTimelockView() (v1_0.MCMSWithTimelockView, error) {
	if err := state.Validate(); err != nil {
		return v1_0.MCMSWithTimelockView{}, err
	}
	timelockView, err := v1_0.GenerateTimelockView(*state.Timelock)
	if err != nil {
		return v1_0.MCMSWithTimelockView{}, nil
	}
	bypasserView, err := v1_0.GenerateMCMSView(*state.BypasserMcm)
	if err != nil {
		return v1_0.MCMSWithTimelockView{}, nil
	}
	proposerView, err := v1_0.GenerateMCMSView(*state.ProposerMcm)
	if err != nil {
		return v1_0.MCMSWithTimelockView{}, nil
	}
	cancellerView, err := v1_0.GenerateMCMSView(*state.CancellerMcm)
	if err != nil {
		return v1_0.MCMSWithTimelockView{}, nil
	}
	return v1_0.MCMSWithTimelockView{
		Timelock:  timelockView,
		Bypasser:  bypasserView,
		Proposer:  proposerView,
		Canceller: cancellerView,
	}, nil
}

// MaybeLoadMCMSWithTimelockState looks for the addresses corresponding to
// contracts deployed with DeployMCMSWithTimelock and loads them into a
// MCMSWithTimelockState struct. If none of the contracts are found, the state struct will be nil.
// An error indicates:
// - Found but was unable to load a contract
// - It only found part of the bundle of contracts
// - If found more than one instance of a contract (we expect one bundle in the given addresses)
func MaybeLoadMCMSWithTimelockState(chain deployment.Chain, addresses map[string]deployment.TypeAndVersion) (*MCMSWithTimelockState, error) {
	state := MCMSWithTimelockState{}
	// We expect one of each contract on the chain.
	timelock := deployment.NewTypeAndVersion(types.RBACTimelock, deployment.Version1_0_0)
	proposer := deployment.NewTypeAndVersion(types.ProposerManyChainMultisig, deployment.Version1_0_0)
	canceller := deployment.NewTypeAndVersion(types.CancellerManyChainMultisig, deployment.Version1_0_0)
	bypasser := deployment.NewTypeAndVersion(types.BypasserManyChainMultisig, deployment.Version1_0_0)

	// Ensure we either have the bundle or not.
	_, err := deployment.AddressesContainBundle(addresses,
		map[deployment.TypeAndVersion]struct{}{
			timelock: {}, proposer: {}, canceller: {}, bypasser: {},
		})
	if err != nil {
		return nil, fmt.Errorf("unable to check MCMS contracts on chain %s error: %w", chain.Name(), err)
	}

	for address, tvStr := range addresses {
		switch tvStr {
		case timelock:
			tl, err := owner_helpers.NewRBACTimelock(common.HexToAddress(address), chain.Client)
			if err != nil {
				return nil, err
			}
			state.Timelock = tl
		case proposer:
			mcms, err := owner_helpers.NewManyChainMultiSig(common.HexToAddress(address), chain.Client)
			if err != nil {
				return nil, err
			}
			state.ProposerMcm = mcms
		case bypasser:
			mcms, err := owner_helpers.NewManyChainMultiSig(common.HexToAddress(address), chain.Client)
			if err != nil {
				return nil, err
			}
			state.BypasserMcm = mcms
		case canceller:
			mcms, err := owner_helpers.NewManyChainMultiSig(common.HexToAddress(address), chain.Client)
			if err != nil {
				return nil, err
			}
			state.CancellerMcm = mcms
		}
	}
	return &state, nil
}

type LinkTokenState struct {
	LinkToken *link_token.LinkToken
}

func (s LinkTokenState) GenerateLinkView() (v1_0.LinkTokenView, error) {
	if s.LinkToken == nil {
		return v1_0.LinkTokenView{}, errors.New("link token not found")
	}
	return v1_0.GenerateLinkTokenView(s.LinkToken)
}

func MaybeLoadLinkTokenState(chain deployment.Chain, addresses map[string]deployment.TypeAndVersion) (*LinkTokenState, error) {
	state := LinkTokenState{}
	linkToken := deployment.NewTypeAndVersion(types.LinkToken, deployment.Version1_0_0)
	// Perhaps revisit if we have a use case for multiple.
	_, err := deployment.AddressesContainBundle(addresses, map[deployment.TypeAndVersion]struct{}{linkToken: {}})
	if err != nil {
		return nil, fmt.Errorf("unable to check link token on chain %s error: %w", chain.Name(), err)
	}
	for address, tvStr := range addresses {
		switch tvStr {
		case linkToken:
			lt, err := link_token.NewLinkToken(common.HexToAddress(address), chain.Client)
			if err != nil {
				return nil, err
			}
			state.LinkToken = lt
		}
	}
	return &state, nil
}

type StaticLinkTokenState struct {
	StaticLinkToken *link_token_interface.LinkToken
}

func (s StaticLinkTokenState) GenerateStaticLinkView() (v1_0.StaticLinkTokenView, error) {
	if s.StaticLinkToken == nil {
		return v1_0.StaticLinkTokenView{}, errors.New("static link token not found")
	}
	return v1_0.GenerateStaticLinkTokenView(s.StaticLinkToken)
}

func MaybeLoadStaticLinkTokenState(chain deployment.Chain, addresses map[string]deployment.TypeAndVersion) (*StaticLinkTokenState, error) {
	state := StaticLinkTokenState{}
	staticLinkToken := deployment.NewTypeAndVersion(types.StaticLinkToken, deployment.Version1_0_0)
	// Perhaps revisit if we have a use case for multiple.
	_, err := deployment.AddressesContainBundle(addresses, map[deployment.TypeAndVersion]struct{}{staticLinkToken: {}})
	if err != nil {
		return nil, fmt.Errorf("unable to check static link token on chain %s error: %w", chain.Name(), err)
	}
	for address, tvStr := range addresses {
		switch tvStr {
		case staticLinkToken:
			lt, err := link_token_interface.NewLinkToken(common.HexToAddress(address), chain.Client)
			if err != nil {
				return nil, err
			}
			state.StaticLinkToken = lt
		}
	}
	return &state, nil
}
