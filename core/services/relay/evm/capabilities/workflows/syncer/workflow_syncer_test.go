package workflow_registry_syncer_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/assert"

	"github.com/smartcontractkit/chainlink-common/pkg/capabilities"
	"github.com/smartcontractkit/chainlink-common/pkg/custmsg"
	"github.com/smartcontractkit/chainlink-common/pkg/services"
	"github.com/smartcontractkit/chainlink-common/pkg/services/servicetest"
	"github.com/smartcontractkit/chainlink-common/pkg/types"
	"github.com/smartcontractkit/chainlink-common/pkg/workflows"
	"github.com/smartcontractkit/chainlink/v2/core/gethwrappers/workflow/generated/workflow_registry_wrapper"
	coretestutils "github.com/smartcontractkit/chainlink/v2/core/internal/testutils"
	"github.com/smartcontractkit/chainlink/v2/core/internal/testutils/pgtest"
	"github.com/smartcontractkit/chainlink/v2/core/logger"
	"github.com/smartcontractkit/chainlink/v2/core/services/keystore/keys/workflowkey"
	"github.com/smartcontractkit/chainlink/v2/core/services/relay/evm/capabilities/testutils"
	"github.com/smartcontractkit/chainlink/v2/core/services/workflows/syncer"
	"github.com/smartcontractkit/chainlink/v2/core/utils/crypto"

	"github.com/stretchr/testify/require"
)

type testEvtHandler struct {
	events []syncer.Event
}

func (m *testEvtHandler) Handle(ctx context.Context, event syncer.Event) error {
	m.events = append(m.events, event)
	return nil
}

func newTestEvtHandler() *testEvtHandler {
	return &testEvtHandler{
		events: make([]syncer.Event, 0),
	}
}

type testWorkflowRegistryContractLoader struct {
}

type testDonNotifier struct {
	don capabilities.DON
	err error
}

func (t *testDonNotifier) WaitForDon(ctx context.Context) (capabilities.DON, error) {
	return t.don, t.err
}

func (m *testWorkflowRegistryContractLoader) LoadWorkflows(ctx context.Context, don capabilities.DON) (*types.Head, error) {
	return &types.Head{
		Height:    "0",
		Hash:      nil,
		Timestamp: 0,
	}, nil
}

func Test_InitialStateSync(t *testing.T) {
	lggr := logger.TestLogger(t)
	backendTH := testutils.NewEVMBackendTH(t)
	donID := uint32(1)

	// Deploy a test workflow_registry
	wfRegistryAddr, _, wfRegistryC, err := workflow_registry_wrapper.DeployWorkflowRegistry(backendTH.ContractsOwner, backendTH.Backend.Client())
	backendTH.Backend.Commit()
	require.NoError(t, err)

	// setup contract state to allow the secrets to be updated
	updateAllowedDONs(t, backendTH, wfRegistryC, []uint32{donID}, true)
	updateAuthorizedAddress(t, backendTH, wfRegistryC, []common.Address{backendTH.ContractsOwner.From}, true)

	// The number of workflows should be greater than the workflow registry contracts pagination limit to ensure
	// that the syncer will query the contract multiple times to get the full list of workflows
	numberWorkflows := 250
	for i := 0; i < numberWorkflows; i++ {
		var workflowID [32]byte
		_, err = rand.Read((workflowID)[:])
		require.NoError(t, err)
		workflow := RegisterWorkflowCMD{
			Name:       fmt.Sprintf("test-wf-%d", i),
			DonID:      donID,
			Status:     uint8(1),
			SecretsURL: "someurl",
		}
		workflow.ID = workflowID
		registerWorkflow(t, backendTH, wfRegistryC, workflow)
	}

	testEventHandler := newTestEvtHandler()
	loader := syncer.NewWorkflowRegistryContractLoader(lggr, wfRegistryAddr.Hex(), func(ctx context.Context, bytes []byte) (syncer.ContractReader, error) {
		return backendTH.NewContractReader(ctx, t, bytes)
	}, testEventHandler)

	// Create the worker
	worker := syncer.NewWorkflowRegistry(
		lggr,
		func(ctx context.Context, bytes []byte) (syncer.ContractReader, error) {
			return backendTH.NewContractReader(ctx, t, bytes)
		},
		wfRegistryAddr.Hex(),
		syncer.WorkflowEventPollerConfig{
			QueryCount: 20,
		},
		testEventHandler,
		loader,
		&testDonNotifier{
			don: capabilities.DON{
				ID: donID,
			},
			err: nil,
		},
		syncer.WithTicker(make(chan time.Time)),
	)

	servicetest.Run(t, worker)

	require.Eventually(t, func() bool {
		return len(testEventHandler.events) == numberWorkflows
	}, 5*time.Second, time.Second)

	for _, event := range testEventHandler.events {
		assert.Equal(t, syncer.WorkflowRegisteredEvent, event.GetEventType())
	}
}

func Test_SecretsWorker(t *testing.T) {
	var (
		ctx       = coretestutils.Context(t)
		lggr      = logger.TestLogger(t)
		emitter   = custmsg.NewLabeler()
		backendTH = testutils.NewEVMBackendTH(t)
		db        = pgtest.NewSqlxDB(t)
		orm       = syncer.NewWorkflowRegistryDS(db, lggr)

		giveTicker     = time.NewTicker(500 * time.Millisecond)
		giveSecretsURL = "https://original-url.com"
		donID          = uint32(1)
		giveWorkflow   = RegisterWorkflowCMD{
			Name:       "test-wf",
			DonID:      donID,
			Status:     uint8(1),
			SecretsURL: giveSecretsURL,
		}
		giveContents = "contents"
		wantContents = "updated contents"
		fetcherFn    = func(_ context.Context, _ string) ([]byte, error) {
			return []byte(wantContents), nil
		}
	)

	defer giveTicker.Stop()

	// fill ID with randomd data
	var giveID [32]byte
	_, err := rand.Read((giveID)[:])
	require.NoError(t, err)
	giveWorkflow.ID = giveID

	// Deploy a test workflow_registry
	wfRegistryAddr, _, wfRegistryC, err := workflow_registry_wrapper.DeployWorkflowRegistry(backendTH.ContractsOwner, backendTH.Backend.Client())
	backendTH.Backend.Commit()
	require.NoError(t, err)

	// Seed the DB
	hash, err := crypto.Keccak256(append(backendTH.ContractsOwner.From[:], []byte(giveSecretsURL)...))
	require.NoError(t, err)
	giveHash := hex.EncodeToString(hash)

	gotID, err := orm.Create(ctx, giveSecretsURL, giveHash, giveContents)
	require.NoError(t, err)

	gotSecretsURL, err := orm.GetSecretsURLByID(ctx, gotID)
	require.NoError(t, err)
	require.Equal(t, giveSecretsURL, gotSecretsURL)

	// verify the DB
	contents, err := orm.GetContents(ctx, giveSecretsURL)
	require.NoError(t, err)
	require.Equal(t, contents, giveContents)

	handler := syncer.NewEventHandler(lggr, orm, fetcherFn, nil, nil,
		emitter, clockwork.NewFakeClock(), workflowkey.Key{})

	worker := syncer.NewWorkflowRegistry(
		lggr,
		func(ctx context.Context, bytes []byte) (syncer.ContractReader, error) {
			return backendTH.NewContractReader(ctx, t, bytes)
		},
		wfRegistryAddr.Hex(),
		syncer.WorkflowEventPollerConfig{QueryCount: 20},
		handler,
		&testWorkflowRegistryContractLoader{},
		&testDonNotifier{
			don: capabilities.DON{
				ID: donID,
			},
			err: nil,
		},
		syncer.WithTicker(giveTicker.C),
	)

	// setup contract state to allow the secrets to be updated
	updateAllowedDONs(t, backendTH, wfRegistryC, []uint32{donID}, true)
	updateAuthorizedAddress(t, backendTH, wfRegistryC, []common.Address{backendTH.ContractsOwner.From}, true)
	registerWorkflow(t, backendTH, wfRegistryC, giveWorkflow)

	servicetest.Run(t, worker)

	// generate a log event
	requestForceUpdateSecrets(t, backendTH, wfRegistryC, giveSecretsURL)

	// Require the secrets contents to eventually be updated
	require.Eventually(t, func() bool {
		secrets, err := orm.GetContents(ctx, giveSecretsURL)
		lggr.Debugf("got secrets %v", secrets)
		require.NoError(t, err)
		return secrets == wantContents
	}, 15*time.Second, time.Second)
}

func Test_RegistrySyncer_WorkflowRegistered_InitiallyPaused(t *testing.T) {
	var (
		ctx       = coretestutils.Context(t)
		lggr      = logger.TestLogger(t)
		emitter   = custmsg.NewLabeler()
		backendTH = testutils.NewEVMBackendTH(t)
		db        = pgtest.NewSqlxDB(t)
		orm       = syncer.NewWorkflowRegistryDS(db, lggr)

		giveTicker    = time.NewTicker(500 * time.Millisecond)
		giveBinaryURL = "https://original-url.com"
		donID         = uint32(1)
		giveWorkflow  = RegisterWorkflowCMD{
			Name:      "test-wf",
			DonID:     donID,
			Status:    uint8(1),
			BinaryURL: giveBinaryURL,
		}
		wantContents = "updated contents"
		fetcherFn    = func(_ context.Context, _ string) ([]byte, error) {
			return []byte(base64.StdEncoding.EncodeToString([]byte(wantContents))), nil
		}
	)

	defer giveTicker.Stop()

	// Deploy a test workflow_registry
	wfRegistryAddr, _, wfRegistryC, err := workflow_registry_wrapper.DeployWorkflowRegistry(backendTH.ContractsOwner, backendTH.Backend.Client())
	backendTH.Backend.Commit()
	require.NoError(t, err)

	from := [20]byte(backendTH.ContractsOwner.From)
	id, err := workflows.GenerateWorkflowID(from[:], []byte(wantContents), []byte(""), "")
	require.NoError(t, err)
	giveWorkflow.ID = id

	er := syncer.NewEngineRegistry()
	handler := syncer.NewEventHandler(lggr, orm, fetcherFn, nil, nil,
		emitter, clockwork.NewFakeClock(), workflowkey.Key{}, syncer.WithEngineRegistry(er))

	worker := syncer.NewWorkflowRegistry(
		lggr,
		func(ctx context.Context, bytes []byte) (syncer.ContractReader, error) {
			return backendTH.NewContractReader(ctx, t, bytes)
		},
		wfRegistryAddr.Hex(),
		syncer.WorkflowEventPollerConfig{QueryCount: 20},
		handler,
		&testWorkflowRegistryContractLoader{},
		&testDonNotifier{
			don: capabilities.DON{
				ID: donID,
			},
			err: nil,
		},
		syncer.WithTicker(giveTicker.C),
	)

	// setup contract state to allow the secrets to be updated
	updateAllowedDONs(t, backendTH, wfRegistryC, []uint32{donID}, true)
	updateAuthorizedAddress(t, backendTH, wfRegistryC, []common.Address{backendTH.ContractsOwner.From}, true)

	servicetest.Run(t, worker)

	// generate a log event
	registerWorkflow(t, backendTH, wfRegistryC, giveWorkflow)

	// Require the secrets contents to eventually be updated
	require.Eventually(t, func() bool {
		_, err = er.Get("test-wf")
		if err == nil {
			return false
		}

		owner := strings.ToLower(backendTH.ContractsOwner.From.Hex()[2:])
		_, err := orm.GetWorkflowSpec(ctx, owner, "test-wf")
		return err == nil
	}, 15*time.Second, time.Second)
}

type mockService struct{}

func (m *mockService) Start(context.Context) error { return nil }

func (m *mockService) Close() error { return nil }

func (m *mockService) HealthReport() map[string]error { return map[string]error{"svc": nil} }

func (m *mockService) Ready() error { return nil }

func (m *mockService) Name() string { return "svc" }

type mockEngineFactory struct{}

func (m *mockEngineFactory) new(ctx context.Context, wfid string, owner string, name string, config []byte, binary []byte) (services.Service, error) {
	return &mockService{}, nil
}

func Test_RegistrySyncer_WorkflowRegistered_InitiallyActivated(t *testing.T) {
	var (
		ctx       = coretestutils.Context(t)
		lggr      = logger.TestLogger(t)
		emitter   = custmsg.NewLabeler()
		backendTH = testutils.NewEVMBackendTH(t)
		db        = pgtest.NewSqlxDB(t)
		orm       = syncer.NewWorkflowRegistryDS(db, lggr)

		giveTicker    = time.NewTicker(500 * time.Millisecond)
		giveBinaryURL = "https://original-url.com"
		donID         = uint32(1)
		giveWorkflow  = RegisterWorkflowCMD{
			Name:      "test-wf",
			DonID:     donID,
			Status:    uint8(0),
			BinaryURL: giveBinaryURL,
		}
		wantContents = "updated contents"
		fetcherFn    = func(_ context.Context, _ string) ([]byte, error) {
			return []byte(base64.StdEncoding.EncodeToString([]byte(wantContents))), nil
		}
	)

	defer giveTicker.Stop()

	// Deploy a test workflow_registry
	wfRegistryAddr, _, wfRegistryC, err := workflow_registry_wrapper.DeployWorkflowRegistry(backendTH.ContractsOwner, backendTH.Backend.Client())
	backendTH.Backend.Commit()
	require.NoError(t, err)

	from := [20]byte(backendTH.ContractsOwner.From)
	id, err := workflows.GenerateWorkflowID(from[:], []byte(wantContents), []byte(""), "")
	require.NoError(t, err)
	giveWorkflow.ID = id

	mf := &mockEngineFactory{}
	er := syncer.NewEngineRegistry()
	handler := syncer.NewEventHandler(
		lggr,
		orm,
		fetcherFn,
		nil,
		nil,
		emitter,
		clockwork.NewFakeClock(),
		workflowkey.Key{},
		syncer.WithEngineRegistry(er),
		syncer.WithEngineFactoryFn(mf.new),
	)

	worker := syncer.NewWorkflowRegistry(
		lggr,
		func(ctx context.Context, bytes []byte) (syncer.ContractReader, error) {
			return backendTH.NewContractReader(ctx, t, bytes)
		},
		wfRegistryAddr.Hex(),
		syncer.WorkflowEventPollerConfig{QueryCount: 20},
		handler,
		&testWorkflowRegistryContractLoader{},
		&testDonNotifier{
			don: capabilities.DON{
				ID: donID,
			},
			err: nil,
		},
		syncer.WithTicker(giveTicker.C),
	)

	// setup contract state to allow the secrets to be updated
	updateAllowedDONs(t, backendTH, wfRegistryC, []uint32{donID}, true)
	updateAuthorizedAddress(t, backendTH, wfRegistryC, []common.Address{backendTH.ContractsOwner.From}, true)

	servicetest.Run(t, worker)

	// generate a log event
	registerWorkflow(t, backendTH, wfRegistryC, giveWorkflow)

	// Require the secrets contents to eventually be updated
	require.Eventually(t, func() bool {
		_, err := er.Get("test-wf")
		if err != nil {
			return err != nil
		}

		owner := strings.ToLower(backendTH.ContractsOwner.From.Hex()[2:])
		_, err = orm.GetWorkflowSpec(ctx, owner, "test-wf")
		return err == nil
	}, 15*time.Second, time.Second)
}

func updateAuthorizedAddress(
	t *testing.T,
	th *testutils.EVMBackendTH,
	wfRegC *workflow_registry_wrapper.WorkflowRegistry,
	addresses []common.Address,
	_ bool,
) {
	t.Helper()
	_, err := wfRegC.UpdateAuthorizedAddresses(th.ContractsOwner, addresses, true)
	require.NoError(t, err, "failed to update authorised addresses")
	th.Backend.Commit()
	th.Backend.Commit()
	th.Backend.Commit()
	gotAddresses, err := wfRegC.GetAllAuthorizedAddresses(&bind.CallOpts{
		From: th.ContractsOwner.From,
	})
	require.NoError(t, err)
	require.ElementsMatch(t, addresses, gotAddresses)
}

func updateAllowedDONs(
	t *testing.T,
	th *testutils.EVMBackendTH,
	wfRegC *workflow_registry_wrapper.WorkflowRegistry,
	donIDs []uint32,
	allowed bool,
) {
	t.Helper()
	_, err := wfRegC.UpdateAllowedDONs(th.ContractsOwner, donIDs, allowed)
	require.NoError(t, err, "failed to update DONs")
	th.Backend.Commit()
	th.Backend.Commit()
	th.Backend.Commit()
	gotDons, err := wfRegC.GetAllAllowedDONs(&bind.CallOpts{
		From: th.ContractsOwner.From,
	})
	require.NoError(t, err)
	require.ElementsMatch(t, donIDs, gotDons)
}

type RegisterWorkflowCMD struct {
	Name       string
	ID         [32]byte
	DonID      uint32
	Status     uint8
	BinaryURL  string
	ConfigURL  string
	SecretsURL string
}

func registerWorkflow(
	t *testing.T,
	th *testutils.EVMBackendTH,
	wfRegC *workflow_registry_wrapper.WorkflowRegistry,
	input RegisterWorkflowCMD,
) {
	t.Helper()
	_, err := wfRegC.RegisterWorkflow(th.ContractsOwner, input.Name, input.ID, input.DonID,
		input.Status, input.BinaryURL, input.ConfigURL, input.SecretsURL)
	require.NoError(t, err, "failed to register workflow")
	th.Backend.Commit()
	th.Backend.Commit()
	th.Backend.Commit()
}

func requestForceUpdateSecrets(
	t *testing.T,
	th *testutils.EVMBackendTH,
	wfRegC *workflow_registry_wrapper.WorkflowRegistry,
	secretsURL string,
) {
	_, err := wfRegC.RequestForceUpdateSecrets(th.ContractsOwner, secretsURL)
	require.NoError(t, err)
	th.Backend.Commit()
	th.Backend.Commit()
	th.Backend.Commit()
}
