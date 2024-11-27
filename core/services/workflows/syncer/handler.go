package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/smartcontractkit/chainlink-common/pkg/custmsg"
	"github.com/smartcontractkit/chainlink-common/pkg/types/core"
	"github.com/smartcontractkit/chainlink-common/pkg/workflows/wasm/host"
	"github.com/smartcontractkit/chainlink/v2/core/logger"
	"github.com/smartcontractkit/chainlink/v2/core/platform"
	"github.com/smartcontractkit/chainlink/v2/core/services/job"
	"github.com/smartcontractkit/chainlink/v2/core/services/workflows"
	"github.com/smartcontractkit/chainlink/v2/core/services/workflows/store"
)

var ErrNotImplemented = errors.New("not implemented")

// WorkflowRegistryrEventType is the type of event that is emitted by the WorkflowRegistry
type WorkflowRegistryEventType string

var (
	// ForceUpdateSecretsEvent is emitted when a request to force update a workflows secrets is made
	ForceUpdateSecretsEvent WorkflowRegistryEventType = "WorkflowForceUpdateSecretsRequestedV1"

	// WorkflowRegisteredEvent is emitted when a workflow is registered
	WorkflowRegisteredEvent WorkflowRegistryEventType = "WorkflowRegisteredV1"

	// WorkflowUpdatedEvent is emitted when a workflow is updated
	WorkflowUpdatedEvent WorkflowRegistryEventType = "WorkflowUpdatedV1"

	// WorkflowPausedEvent is emitted when a workflow is paused
	WorkflowPausedEvent WorkflowRegistryEventType = "WorkflowPausedV1"

	// WorkflowActivatedEvent is emitted when a workflow is activated
	WorkflowActivatedEvent WorkflowRegistryEventType = "WorkflowActivatedV1"

	// WorkflowDeletedEvent is emitted when a workflow is deleted
	WorkflowDeletedEvent WorkflowRegistryEventType = "WorkflowDeletedV1"
)

// WorkflowRegistryForceUpdateSecretsRequestedV1 is a chain agnostic definition of the WorkflowRegistry
// ForceUpdateSecretsRequested event.
type WorkflowRegistryForceUpdateSecretsRequestedV1 struct {
	SecretsURLHash []byte
	Owner          []byte
	WorkflowName   string
}

type WorkflowRegistryWorkflowRegisteredV1 struct {
	WorkflowID    [32]byte
	WorkflowOwner []byte
	DonID         uint32
	Status        uint8
	WorkflowName  string
	BinaryURL     string
	ConfigURL     string
	SecretsURL    string
}

type WorkflowRegistryWorkflowUpdatedV1 struct {
	OldWorkflowID [32]byte
	WorkflowOwner []byte
	DonID         uint32
	NewWorkflowID [32]byte
	WorkflowName  string
	BinaryURL     string
	ConfigURL     string
	SecretsURL    string
}

type WorkflowRegistryWorkflowPausedV1 struct {
	WorkflowID    [32]byte
	WorkflowOwner []byte
	DonID         uint32
	WorkflowName  string
}

type WorkflowRegistryWorkflowActivatedV1 struct {
	WorkflowID    [32]byte
	WorkflowOwner []byte
	DonID         uint32
	WorkflowName  string
}

type WorkflowRegistryWorkflowDeletedV1 struct {
	WorkflowID    [32]byte
	WorkflowOwner []byte
	DonID         uint32
	WorkflowName  string
}

type secretsFetcher interface {
	SecretsFor(ctx context.Context, workflowOwner, workflowName string) (map[string]string, error)
}

// secretsFetcherFunc implements the secretsFetcher interface for a function.
type secretsFetcherFunc func(ctx context.Context, workflowOwner, workflowName string) (map[string]string, error)

func (f secretsFetcherFunc) SecretsFor(ctx context.Context, workflowOwner, workflowName string) (map[string]string, error) {
	return f(ctx, workflowOwner, workflowName)
}

// eventHandler is a handler for WorkflowRegistryEvent events.  Each event type has a corresponding
// method that handles the event.
type eventHandler struct {
	lggr           logger.Logger
	orm            WorkflowRegistryDS
	fetcher        FetcherFunc
	workflowStore  store.Store
	capRegistry    core.CapabilitiesRegistry
	engineRegistry *engineRegistry
	emitter        custmsg.MessageEmitter
	secretsFetcher secretsFetcher
}

// newEventHandler returns a new eventHandler instance.
func newEventHandler(
	lggr logger.Logger,
	orm ORM,
	gateway FetcherFunc,
	workflowStore store.Store,
	capRegistry core.CapabilitiesRegistry,
	engineRegistry *engineRegistry,
	emitter custmsg.MessageEmitter,
	secretsFetcher secretsFetcher,
) *eventHandler {
	return &eventHandler{
		lggr:           lggr,
		orm:            orm,
		fetcher:        gateway,
		workflowStore:  workflowStore,
		capRegistry:    capRegistry,
		engineRegistry: engineRegistry,
		emitter:        emitter,
		secretsFetcher: secretsFetcher,
	}
}

func (h *eventHandler) Handle(ctx context.Context, event WorkflowRegistryEvent) error {
	switch event.EventType {
	case ForceUpdateSecretsEvent:
		payload, ok := event.Data.(WorkflowRegistryForceUpdateSecretsRequestedV1)
		if !ok {
			return newHandlerTypeError(event.Data)
		}

		cma := h.emitter.With(
			platform.KeyWorkflowName, payload.WorkflowName,
			platform.KeyWorkflowOwner, hex.EncodeToString(payload.Owner),
		)

		if err := h.forceUpdateSecretsEvent(ctx, payload); err != nil {
			logCustMsg(ctx, cma, fmt.Sprintf("failed to handle force update secrets event: %v", err), h.lggr)
			return err
		}

		return nil
	case WorkflowRegisteredEvent:
		payload, ok := event.Data.(WorkflowRegistryWorkflowRegisteredV1)
		if !ok {
			return newHandlerTypeError(event.Data)
		}
		wfID := hex.EncodeToString(payload.WorkflowID[:])

		cma := h.emitter.With(
			platform.KeyWorkflowID, wfID,
			platform.KeyWorkflowName, payload.WorkflowName,
			platform.KeyWorkflowOwner, hex.EncodeToString(payload.WorkflowOwner),
		)

		if err := h.workflowRegisteredEvent(ctx, payload); err != nil {
			logCustMsg(ctx, cma, fmt.Sprintf("failed to handle workflow registered event: %v", err), h.lggr)
			return err
		}

		h.lggr.Debugf("workflow 0x%x registered and started", wfID)
		return nil
	case WorkflowUpdatedEvent:
		payload, ok := event.Data.(WorkflowRegistryWorkflowUpdatedV1)
		if !ok {
			return fmt.Errorf("invalid data type %T for event", event.Data)
		}

		newWorkflowID := hex.EncodeToString(payload.NewWorkflowID[:])
		cma := h.emitter.With(
			platform.KeyWorkflowID, newWorkflowID,
			platform.KeyWorkflowName, payload.WorkflowName,
			platform.KeyWorkflowOwner, hex.EncodeToString(payload.WorkflowOwner),
		)

		if err := h.workflowUpdatedEvent(ctx, payload); err != nil {
			logCustMsg(ctx, cma, fmt.Sprintf("failed to handle workflow updated event: %v", err), h.lggr)
			return err
		}

		return nil
	case WorkflowPausedEvent:
		payload, ok := event.Data.(WorkflowRegistryWorkflowPausedV1)
		if !ok {
			return fmt.Errorf("invalid data type %T for event", event.Data)
		}

		wfID := hex.EncodeToString(payload.WorkflowID[:])

		cma := h.emitter.With(
			platform.KeyWorkflowID, wfID,
			platform.KeyWorkflowName, payload.WorkflowName,
			platform.KeyWorkflowOwner, hex.EncodeToString(payload.WorkflowOwner),
		)

		if err := h.workflowPausedEvent(ctx, payload); err != nil {
			logCustMsg(ctx, cma, fmt.Sprintf("failed to handle workflow paused event: %v", err), h.lggr)
			return err
		}
		return nil
	case WorkflowActivatedEvent:
		payload, ok := event.Data.(WorkflowRegistryWorkflowActivatedV1)
		if !ok {
			return fmt.Errorf("invalid data type %T for event", event.Data)
		}

		wfID := hex.EncodeToString(payload.WorkflowID[:])

		cma := h.emitter.With(
			platform.KeyWorkflowID, wfID,
			platform.KeyWorkflowName, payload.WorkflowName,
			platform.KeyWorkflowOwner, hex.EncodeToString(payload.WorkflowOwner),
		)
		if err := h.workflowActivatedEvent(ctx, payload); err != nil {
			logCustMsg(ctx, cma, fmt.Sprintf("failed to handle workflow activated event: %v", err), h.lggr)
			return err
		}

		return nil
	case WorkflowDeletedEvent:
		payload, ok := event.Data.(WorkflowRegistryWorkflowDeletedV1)
		if !ok {
			return fmt.Errorf("invalid data type %T for event", event.Data)
		}

		wfID := hex.EncodeToString(payload.WorkflowID[:])

		cma := h.emitter.With(
			platform.KeyWorkflowID, wfID,
			platform.KeyWorkflowName, payload.WorkflowName,
			platform.KeyWorkflowOwner, hex.EncodeToString(payload.WorkflowOwner),
		)

		if err := h.workflowDeletedEvent(ctx, payload); err != nil {
			logCustMsg(ctx, cma, fmt.Sprintf("failed to handle workflow deleted event: %v", err), h.lggr)
			return err
		}

		return nil
	default:
		return fmt.Errorf("event type unsupported: %v", event.EventType)
	}
}

// workflowRegisteredEvent handles the WorkflowRegisteredEvent event type.
func (h *eventHandler) workflowRegisteredEvent(
	ctx context.Context,
	payload WorkflowRegistryWorkflowRegisteredV1,
) error {
	wfID := hex.EncodeToString(payload.WorkflowID[:])

	// Download the contents of binaryURL, configURL and secretsURL and cache them locally.
	binary, err := h.fetcher(ctx, payload.BinaryURL)
	if err != nil {
		return fmt.Errorf("failed to fetch binary from %s : %w", payload.BinaryURL, err)
	}

	config, err := h.fetcher(ctx, payload.ConfigURL)
	if err != nil {
		return fmt.Errorf("failed to fetch config from %s : %w", payload.ConfigURL, err)
	}

	secrets, err := h.fetcher(ctx, payload.SecretsURL)
	if err != nil {
		return fmt.Errorf("failed to fetch secrets from %s : %w", payload.SecretsURL, err)
	}

	// Calculate the hash of the binary and config files
	hash := workflowID(binary, config, []byte(payload.SecretsURL))

	// Pre-check: verify that the workflowID matches; if it doesn’t abort and log an error via Beholder.
	if hash != wfID {
		return fmt.Errorf("workflowID mismatch: %s != %s", hash, wfID)
	}

	// Save the workflow secrets
	urlHash, err := h.orm.GetSecretsURLHash(payload.WorkflowOwner, []byte(payload.SecretsURL))
	if err != nil {
		return fmt.Errorf("failed to get secrets URL hash: %w", err)
	}

	// Create a new entry in the workflow_spec table corresponding for the new workflow, with the contents of the binaryURL + configURL in the table
	status := job.WorkflowSpecStatusActive
	if payload.Status == 1 {
		status = job.WorkflowSpecStatusPaused
	}

	entry := &job.WorkflowSpec{
		Workflow:      hex.EncodeToString(binary),
		Config:        string(config),
		WorkflowID:    wfID,
		Status:        status,
		WorkflowOwner: hex.EncodeToString(payload.WorkflowOwner),
		WorkflowName:  payload.WorkflowName,
		SpecType:      job.WASMFile,
		BinaryURL:     payload.BinaryURL,
		ConfigURL:     payload.ConfigURL,
	}
	if _, err = h.orm.UpsertWorkflowSpecWithSecrets(ctx, entry, payload.SecretsURL, hex.EncodeToString(urlHash), string(secrets)); err != nil {
		return fmt.Errorf("failed to upsert workflow spec with secrets: %w", err)
	}

	if status != job.WorkflowSpecStatusActive {
		return nil
	}

	// If status == active, start a new WorkflowEngine instance, and add it to local engine registry
	moduleConfig := &host.ModuleConfig{Logger: h.lggr, Labeler: h.emitter}
	sdkSpec, err := host.GetWorkflowSpec(ctx, moduleConfig, binary, config)
	if err != nil {
		return fmt.Errorf("failed to get workflow sdk spec: %w", err)
	}

	cfg := workflows.Config{
		Lggr:           h.lggr,
		Workflow:       *sdkSpec,
		WorkflowID:     wfID,
		WorkflowOwner:  hex.EncodeToString(payload.WorkflowOwner),
		WorkflowName:   payload.WorkflowName,
		Registry:       h.capRegistry,
		Store:          h.workflowStore,
		Config:         config,
		Binary:         binary,
		SecretsFetcher: h.secretsFetcher,
	}
	e, err := workflows.NewEngine(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to create workflow engine: %w", err)
	}

	if err := e.Start(ctx); err != nil {
		return fmt.Errorf("failed to start workflow engine: %w", err)
	}

	h.engineRegistry.Add(wfID, e)
	return nil
}

// workflowUpdatedEvent handles the WorkflowUpdatedEvent event type by first finding the
// current workflow engine, stopping it, and then starting a new workflow engine with the
// updated workflow spec.
func (h *eventHandler) workflowUpdatedEvent(
	ctx context.Context,
	payload WorkflowRegistryWorkflowUpdatedV1,
) error {
	// Remove the old workflow engine from the local registry if it exists
	if err := h.tryEngineCleanup(hex.EncodeToString(payload.OldWorkflowID[:])); err != nil {
		return err
	}

	registeredEvent := WorkflowRegistryWorkflowRegisteredV1{
		WorkflowID:    payload.NewWorkflowID,
		WorkflowOwner: payload.WorkflowOwner,
		DonID:         payload.DonID,
		Status:        0,
		WorkflowName:  payload.WorkflowName,
		BinaryURL:     payload.BinaryURL,
		ConfigURL:     payload.ConfigURL,
		SecretsURL:    payload.SecretsURL,
	}

	return h.workflowRegisteredEvent(ctx, registeredEvent)
}

// workflowPausedEvent handles the WorkflowPausedEvent event type.
func (h *eventHandler) workflowPausedEvent(
	ctx context.Context,
	payload WorkflowRegistryWorkflowPausedV1,
) error {
	// Remove the workflow engine from the local registry if it exists
	if err := h.tryEngineCleanup(hex.EncodeToString(payload.WorkflowID[:])); err != nil {
		return err
	}

	// get existing workflow spec from DB
	spec, err := h.orm.GetWorkflowSpec(ctx, hex.EncodeToString(payload.WorkflowOwner), payload.WorkflowName)
	if err != nil {
		return fmt.Errorf("failed to get workflow spec: %w", err)
	}

	// update the status of the workflow spec
	spec.Status = job.WorkflowSpecStatusPaused
	if _, err := h.orm.UpsertWorkflowSpec(ctx, spec); err != nil {
		return fmt.Errorf("failed to update workflow spec: %w", err)
	}

	return nil
}

// workflowActivatedEvent handles the WorkflowActivatedEvent event type.
func (h *eventHandler) workflowActivatedEvent(
	ctx context.Context,
	payload WorkflowRegistryWorkflowActivatedV1,
) error {
	// fetch the workflow spec from the DB
	spec, err := h.orm.GetWorkflowSpec(ctx, hex.EncodeToString(payload.WorkflowOwner), payload.WorkflowName)
	if err != nil {
		return fmt.Errorf("failed to get workflow spec: %w", err)
	}

	// Do nothing if the workflow is already active
	if spec.Status == job.WorkflowSpecStatusActive && h.engineRegistry.IsRunning(hex.EncodeToString(payload.WorkflowID[:])) {
		return nil
	}

	// get the secrets url by the secrets id
	secretsURL, err := h.orm.GetSecretsURLByID(ctx, spec.SecretsID.Int64)
	if err != nil {
		return fmt.Errorf("failed to get secrets URL by ID: %w", err)
	}

	// start a new workflow engine
	registeredEvent := WorkflowRegistryWorkflowRegisteredV1{
		WorkflowID:    payload.WorkflowID,
		WorkflowOwner: payload.WorkflowOwner,
		DonID:         payload.DonID,
		Status:        0,
		WorkflowName:  payload.WorkflowName,
		BinaryURL:     spec.BinaryURL,
		ConfigURL:     spec.ConfigURL,
		SecretsURL:    secretsURL,
	}

	return h.workflowRegisteredEvent(ctx, registeredEvent)
}

// workflowDeletedEvent handles the WorkflowDeletedEvent event type.
func (h *eventHandler) workflowDeletedEvent(
	ctx context.Context,
	payload WorkflowRegistryWorkflowDeletedV1,
) error {
	if err := h.tryEngineCleanup(hex.EncodeToString(payload.WorkflowID[:])); err != nil {
		return err
	}

	if err := h.orm.DeleteWorkflowSpec(ctx, hex.EncodeToString(payload.WorkflowOwner), payload.WorkflowName); err != nil {
		return fmt.Errorf("failed to delete workflow spec: %w", err)
	}
	return nil
}

// forceUpdateSecretsEvent handles the ForceUpdateSecretsEvent event type.
func (h *eventHandler) forceUpdateSecretsEvent(
	ctx context.Context,
	payload WorkflowRegistryForceUpdateSecretsRequestedV1,
) error {
	// Get the URL of the secrets file from the event data
	hash := hex.EncodeToString(payload.SecretsURLHash)

	url, err := h.orm.GetSecretsURLByHash(ctx, hash)
	if err != nil {
		return fmt.Errorf("failed to get URL by hash %s : %w", hash, err)
	}

	// Fetch the contents of the secrets file from the url via the fetcher
	secrets, err := h.fetcher(ctx, url)
	if err != nil {
		return fmt.Errorf("failed to fetch secrets from url %s : %w", url, err)
	}

	// Update the secrets in the ORM
	if _, err := h.orm.Update(ctx, hash, string(secrets)); err != nil {
		return fmt.Errorf("failed to update secrets: %w", err)
	}

	return nil
}

// tryEngineCleanup attempts to stop the workflow engine for the given workflow ID.  Does nothing if the
// workflow engine is not running.
func (h *eventHandler) tryEngineCleanup(wfID string) error {
	if h.engineRegistry.IsRunning(wfID) {
		// Remove the engine from the registry
		e, err := h.engineRegistry.Pop(wfID)
		if err != nil {
			return fmt.Errorf("failed to get workflow engine: %w", err)
		}

		// Stop the engine
		if err := e.Close(); err != nil {
			return fmt.Errorf("failed to close workflow engine: %w", err)
		}
	}
	return nil
}

// workflowID returns a hex encoded sha256 hash of the wasm, config and secretsURL.
func workflowID(wasm, config, secretsURL []byte) string {
	sum := sha256.New()
	sum.Write(wasm)
	sum.Write(config)
	sum.Write(secretsURL)
	return hex.EncodeToString(sum.Sum(nil))
}

// logCustMsg emits a custom message to the external sink and logs an error if that fails.
func logCustMsg(ctx context.Context, cma custmsg.MessageEmitter, msg string, log logger.Logger) {
	err := cma.Emit(ctx, msg)
	if err != nil {
		log.Helper(1).Errorf("failed to send custom message with msg: %s, err: %v", msg, err)
	}
}

func newHandlerTypeError(data any) error {
	return fmt.Errorf("invalid data type %T for event", data)
}