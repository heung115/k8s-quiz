package session

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/k8s-quiz/backend/internal/runner"
	"github.com/k8s-quiz/backend/pkg/models"
)

type fakeDurableStore struct {
	mu              sync.Mutex
	reservation     runner.SessionReservation
	reservations    map[string]runner.SessionReservation
	pendingDestroy  map[string]runner.AllocationRef
	destroyClaims   map[string]runner.DestroyWorkClaim
	destroyAttempts map[string]int
	createClaims    map[string]runner.CreateWorkClaim
	reserveErr      error
	markCreateErr   error
	markReadyErr    error
	lookupVerifyErr error
	beginVerifyErr  error
	verifyFinishErr error
	verifyInfraErr  error
	choiceRecordErr error
	findCalls       int
	reserveCalls    int
	createSuccess   int
	settingUpCalls  int
	readyCalls      int
	verifyStarts    int
	verifyFinishes  int
	verifyInfra     int
	verifyDecisions map[string]runner.VerifyDecision
	choiceRequests  map[string]runner.ChoiceSubmission
	endRequests     map[string]runner.SessionRef
	resetCalls      int
	createFailures  int
	destroyRequest  int
	destroyedCalls  int
	order           []string
	reserveHook     func(runner.ReserveSessionParams) error
	endCommitHook   func()
	endReturnErr    error
}

func fakeVerifyKey(userID, operationID string) string { return userID + "\x00" + operationID }

func (f *fakeDurableStore) LookupVerify(_ context.Context, userID, operationID string) (runner.VerifyDecision, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lookupVerifyErr != nil {
		return runner.VerifyDecision{}, false, f.lookupVerifyErr
	}
	decision, found := f.verifyDecisions[fakeVerifyKey(userID, operationID)]
	if found {
		f.ensureReservations()
		for _, reservation := range f.reservations {
			if reservation.Session.UserID == userID && reservation.Session.DesiredState == "active" &&
				reservation.Allocation.DesiredState == "active" &&
				reservation.Allocation.Ref.Session.Generation == reservation.Session.CurrentGeneration &&
				reservation.Allocation.Ref.Session != decision.Session {
				return runner.VerifyDecision{}, false, runner.ErrIdempotencyConflict
			}
		}
	}
	return decision, found, nil
}

func (f *fakeDurableStore) LookupChoice(_ context.Context, userID string, submission runner.ChoiceSubmission) (runner.VerifyDecision, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureReservations()
	key := fakeVerifyKey(userID, submission.OperationID)
	decision, found := f.verifyDecisions[key]
	if !found {
		return runner.VerifyDecision{}, false, nil
	}
	stored, isChoice := f.choiceRequests[key]
	if !isChoice || stored != submission || decision.Session != submission.Session {
		return runner.VerifyDecision{}, false, runner.ErrIdempotencyConflict
	}
	return decision, true, nil
}

func (f *fakeDurableStore) RecordChoice(_ context.Context, userID string, ref runner.AllocationRef, submission runner.ChoiceSubmission, success bool, verifyLog string) (runner.VerifyDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.choiceRecordErr != nil {
		return runner.VerifyDecision{}, f.choiceRecordErr
	}
	f.ensureReservations()
	key := fakeVerifyKey(userID, submission.OperationID)
	if prior, found := f.verifyDecisions[key]; found {
		stored, isChoice := f.choiceRequests[key]
		if !isChoice || stored != submission || prior.Session != ref.Session {
			return runner.VerifyDecision{}, runner.ErrIdempotencyConflict
		}
		return prior, nil
	}
	if submission.Session != ref.Session {
		return runner.VerifyDecision{}, runner.ErrIdempotencyConflict
	}
	valid := false
	for _, reservation := range f.reservations {
		if reservation.Allocation.Ref == ref && reservation.Session.UserID == userID &&
			reservation.Session.Selection.Problem.ID == submission.ProblemID &&
			reservation.Session.CurrentGeneration == submission.Session.Generation &&
			reservation.Session.State == "ready" && reservation.Session.DesiredState == "active" &&
			reservation.Allocation.DesiredState == "active" && reservation.Allocation.ObservedState == "running" &&
			reservation.AttemptStatus == "in_progress" {
			valid = true
			break
		}
	}
	if !valid {
		return runner.VerifyDecision{}, runner.ErrGenerationStale
	}
	f.verifyStarts++
	f.verifyFinishes++
	f.order = append(f.order, "verify_started", "verify_finished")
	if !f.updateReservation(ref, func(reservation *runner.SessionReservation) {
		reservation.Allocation.LastEventSequence += 2
		if success {
			reservation.Allocation.LastEventSequence++
			reservation.Session.State = "completed"
			reservation.Session.DesiredState = "absent"
			reservation.Allocation.DesiredState = "absent"
			reservation.AttemptStatus = "success"
		}
	}) {
		return runner.VerifyDecision{}, runner.ErrAllocationNotFound
	}
	if success {
		f.pendingDestroy[ref.ID] = ref
	}
	decision := runner.VerifyDecision{
		Kind: runner.VerifyGradeReplay, Session: ref.Session, Success: success, Log: boundedVerifyLog(verifyLog),
	}
	f.choiceRequests[key] = submission
	f.verifyDecisions[key] = decision
	return decision, nil
}

func (f *fakeDurableStore) BeginVerify(_ context.Context, userID string, ref runner.AllocationRef, operationID string) (runner.VerifyDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.beginVerifyErr != nil {
		return runner.VerifyDecision{}, f.beginVerifyErr
	}
	if operationID == "" {
		return runner.VerifyDecision{}, errors.New("verify operation id is required")
	}
	f.ensureReservations()
	key := fakeVerifyKey(userID, operationID)
	if decision, found := f.verifyDecisions[key]; found {
		if decision.Session != ref.Session {
			return runner.VerifyDecision{}, runner.ErrIdempotencyConflict
		}
		return decision, nil
	}
	var matched *runner.SessionReservation
	for _, reservation := range f.reservations {
		if reservation.Allocation.Ref == ref {
			candidate := reservation
			matched = &candidate
			break
		}
	}
	if matched == nil {
		return runner.VerifyDecision{}, runner.ErrAllocationNotFound
	}
	if matched.Session.UserID != userID {
		return runner.VerifyDecision{}, runner.ErrIdempotencyConflict
	}
	if matched.Session.CurrentGeneration != ref.Session.Generation {
		return runner.VerifyDecision{}, runner.ErrGenerationStale
	}
	if matched.Session.DesiredState != "active" || matched.Allocation.DesiredState != "active" {
		return runner.VerifyDecision{}, runner.ErrGenerationStale
	}
	if matched.Allocation.ObservedState != "running" || matched.Session.State != "ready" || matched.AttemptStatus != "in_progress" {
		return runner.VerifyDecision{}, runner.ErrLifecycleConflict
	}
	f.updateReservation(ref, func(reservation *runner.SessionReservation) {
		reservation.Allocation.LastEventSequence++
		reservation.Session.State = "verifying"
	})
	f.verifyStarts++
	f.order = append(f.order, "verify_started")
	f.verifyDecisions[key] = runner.VerifyDecision{Kind: runner.VerifyResume, Session: ref.Session}
	return runner.VerifyDecision{Kind: runner.VerifyExecute, Session: ref.Session}, nil
}

func (f *fakeDurableStore) BootstrapLifecycle(_ context.Context, userID string, _ *runner.LifecycleCursor) (runner.LifecycleBootstrap, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureReservations()
	var current *runner.SessionReservation
	for _, reservation := range f.reservations {
		if reservation.Session.UserID != userID || reservation.Session.DesiredState != "active" ||
			reservation.Allocation.DesiredState != "active" ||
			reservation.Allocation.Ref.Session.Generation != reservation.Session.CurrentGeneration {
			continue
		}
		candidate := reservation
		if current == nil || candidate.Session.CurrentGeneration > current.Session.CurrentGeneration {
			current = &candidate
		}
	}
	if current == nil {
		return runner.LifecycleBootstrap{}, nil
	}
	cleanupPending := false
	for _, reservation := range f.reservations {
		if reservation.Session.ID == current.Session.ID &&
			reservation.Allocation.Ref.Session.Generation < current.Allocation.Ref.Session.Generation &&
			(reservation.Allocation.DesiredState != "absent" || reservation.Allocation.ObservedState != "absent") {
			cleanupPending = true
			break
		}
	}
	snapshot := &runner.LifecycleSnapshot{
		SessionID: current.Session.ID, ProblemID: current.Session.Selection.Problem.ID,
		Generation: current.Allocation.Ref.Session.Generation, OperationID: current.Operation.ID,
		Status: current.Session.State, TimeoutAt: current.Allocation.ExpiresAt,
		CleanupPending: cleanupPending, EventSequence: current.Allocation.LastEventSequence,
	}
	return runner.LifecycleBootstrap{Snapshot: snapshot}, nil
}

func (f *fakeDurableStore) ReserveReset(_ context.Context, params runner.ReserveResetParams) (runner.ResetReservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetCalls++
	f.order = append(f.order, "reserve_reset")
	if f.reserveErr != nil {
		return runner.ResetReservation{}, f.reserveErr
	}
	f.ensureReservations()
	if replay, found := f.reservations[params.IdempotencyKey]; found {
		old := runner.AllocationRef{
			ID: runner.AllocationIDForSession(params.Expected), Session: params.Expected, Provider: params.Provider,
		}
		return runner.ResetReservation{Old: old, New: replay}, nil
	}
	old := f.reservation.Allocation.Ref
	f.updateReservation(old, func(reservation *runner.SessionReservation) {
		reservation.Session.DesiredState = "absent"
		reservation.Allocation.DesiredState = "absent"
	})
	f.pendingDestroy[old.ID] = old
	newSession := runner.SessionRef{SessionID: params.Expected.SessionID, Generation: params.Expected.Generation + 1}
	newRef := runner.AllocationRef{ID: runner.AllocationIDForSession(newSession), Session: newSession, Provider: params.Provider}
	newReservation := f.reservation
	newReservation.Session.CurrentGeneration = newSession.Generation
	newReservation.Session.State = "queued"
	newReservation.Session.DesiredState = "active"
	newReservation.Allocation.Ref = newRef
	newReservation.Allocation.DesiredState = "active"
	newReservation.Allocation.ObservedState = "unknown"
	newReservation.Allocation.LastEventSequence = 2
	newReservation.Operation = runner.DurableOperation{
		ID: runner.SessionIDForOperation(params.UserID, "operation:"+params.IdempotencyKey), AllocationID: newRef.ID,
		ProviderID: params.ProviderID, Kind: "create", IdempotencyKey: params.IdempotencyKey, State: "pending",
	}
	newReservation.AttemptID = "33333333-3333-4333-8333-333333333333"
	f.reservation = newReservation
	f.reservations[params.IdempotencyKey] = newReservation
	return runner.ResetReservation{Old: old, New: newReservation}, nil
}

func (f *fakeDurableStore) FindReservation(_ context.Context, providerID, key string) (runner.SessionReservation, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.findCalls++
	f.order = append(f.order, "find")
	f.ensureReservations()
	reservation, found := f.reservations[key]
	return reservation, found, nil
}

func (f *fakeDurableStore) ListActiveReservations(_ context.Context, _ string) ([]runner.SessionReservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureReservations()
	bySession := make(map[string]runner.SessionReservation)
	for _, reservation := range f.reservations {
		if reservation.Session.DesiredState != "active" || reservation.Allocation.DesiredState != "active" ||
			reservation.Allocation.Ref.Session.Generation != reservation.Session.CurrentGeneration {
			continue
		}
		reservation.AttemptStatus = "in_progress"
		bySession[reservation.Session.ID] = reservation
	}
	result := make([]runner.SessionReservation, 0, len(bySession))
	for _, reservation := range bySession {
		result = append(result, reservation)
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].Session.QueuedAt.Equal(result[j].Session.QueuedAt) {
			return result[i].Session.QueuedAt.Before(result[j].Session.QueuedAt)
		}
		return result[i].Session.ID < result[j].Session.ID
	})
	return result, nil
}

func (f *fakeDurableStore) PrepareRecoveryWork(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureReservations()
	for key, reservation := range f.reservations {
		if reservation.Operation.State == "running" {
			reservation.Operation.State = "pending"
			f.reservations[key] = reservation
			if f.reservation.Operation.ID == reservation.Operation.ID {
				f.reservation = reservation
			}
		}
	}
	f.createClaims = make(map[string]runner.CreateWorkClaim)
	f.destroyClaims = make(map[string]runner.DestroyWorkClaim)
	return nil
}

func (f *fakeDurableStore) ReserveSession(_ context.Context, params runner.ReserveSessionParams) (runner.SessionReservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserveCalls++
	f.order = append(f.order, "reserve")
	if f.reserveHook != nil {
		if err := f.reserveHook(params); err != nil {
			return runner.SessionReservation{}, err
		}
	}
	if f.reserveErr != nil {
		return runner.SessionReservation{}, f.reserveErr
	}
	f.ensureReservations()
	if replay, found := f.reservations[params.IdempotencyKey]; found {
		return replay, nil
	}
	ref := runner.SessionRef{SessionID: params.SessionID, Generation: 1}
	f.reservation = runner.SessionReservation{
		Session: runner.DurableSession{
			ID: params.SessionID, UserID: params.UserID, Selection: params.Selection,
			CurrentGeneration: 1, State: "queued", DesiredState: "active",
			QueuedAt: time.Now(), ExpiresAt: params.ExpiresAt,
		},
		Allocation: runner.DurableAllocation{
			Ref: runner.AllocationRef{
				ID: runner.AllocationIDForSession(ref), Session: ref, Provider: params.Provider,
			},
			ProviderID: params.ProviderID, ResourceProfile: params.ResourceProfile,
			DesiredState: "active", ObservedState: "unknown", ExpiresAt: params.ExpiresAt,
			LastEventSequence: 1,
		},
		Operation: runner.DurableOperation{
			ID: runner.SessionIDForOperation(params.SessionID, "create:"+params.IdempotencyKey), AllocationID: runner.AllocationIDForSession(ref),
			ProviderID: params.ProviderID, Kind: "create", IdempotencyKey: params.IdempotencyKey, State: "pending",
		},
		AttemptID:     runner.SessionIDForOperation(params.SessionID, "attempt:"+params.IdempotencyKey),
		AttemptStatus: "in_progress",
	}
	f.reservations[params.IdempotencyKey] = f.reservation
	return f.reservation, nil
}

type testActiveProblemCatalog struct {
	mu           sync.Mutex
	active       models.Problem
	history      map[runner.ProblemRef]models.Problem
	activeErr    error
	leaseHeld    bool
	acquireCalls int
	resolveCalls int
}

func newTestActiveProblemCatalog(problem models.Problem) *testActiveProblemCatalog {
	ref := runner.ProblemRef{ID: problem.ID, Revision: problem.Revision}
	return &testActiveProblemCatalog{active: problem, history: map[runner.ProblemRef]models.Problem{ref: problem}}
}

func (c *testActiveProblemCatalog) AcquireActiveProblem(_ context.Context, problemID string) (models.Problem, runner.CatalogSelection, func(), error) {
	c.mu.Lock()
	c.acquireCalls++
	if c.activeErr != nil {
		err := c.activeErr
		c.mu.Unlock()
		return models.Problem{}, runner.CatalogSelection{}, nil, err
	}
	if c.active.ID != problemID || c.active.Revision == "" {
		c.mu.Unlock()
		return models.Problem{}, runner.CatalogSelection{}, nil, runner.ErrInvalidRevision
	}
	c.leaseHeld = true
	problem := c.active
	c.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			c.mu.Lock()
			c.leaseHeld = false
			c.mu.Unlock()
		})
	}
	return problem, runner.CatalogSelection{
		Generation: 1,
		Problem:    runner.ProblemRef{ID: problem.ID, Revision: problem.Revision},
	}, release, nil
}

func (c *testActiveProblemCatalog) ResolveProblem(_ context.Context, ref runner.ProblemRef) (models.Problem, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resolveCalls++
	problem, found := c.history[ref]
	if !found {
		return models.Problem{}, runner.ErrInvalidRevision
	}
	return problem, nil
}

func TestNewDurablePausedServiceRejectsTypedNilProblemCatalog(t *testing.T) {
	var catalog *testActiveProblemCatalog

	svc, err := NewDurablePausedService(nil, nil, catalog, &fakeDurableStore{}, "local-docker:test")
	if err == nil || err.Error() != "durable session service requires an active problem catalog" {
		t.Fatalf("constructor error = %v, want active problem catalog error", err)
	}
	if svc != nil {
		t.Fatal("constructor returned a service for a typed-nil problem catalog")
	}
}

func (c *testActiveProblemCatalog) isLeaseHeld() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.leaseHeld
}

func (f *fakeDurableStore) ensureReservations() {
	if f.reservations == nil {
		f.reservations = make(map[string]runner.SessionReservation)
	}
	if f.pendingDestroy == nil {
		f.pendingDestroy = make(map[string]runner.AllocationRef)
	}
	if f.destroyClaims == nil {
		f.destroyClaims = make(map[string]runner.DestroyWorkClaim)
	}
	if f.destroyAttempts == nil {
		f.destroyAttempts = make(map[string]int)
	}
	if f.createClaims == nil {
		f.createClaims = make(map[string]runner.CreateWorkClaim)
	}
	if f.verifyDecisions == nil {
		f.verifyDecisions = make(map[string]runner.VerifyDecision)
	}
	if f.choiceRequests == nil {
		f.choiceRequests = make(map[string]runner.ChoiceSubmission)
	}
}

func (f *fakeDurableStore) updateReservation(ref runner.AllocationRef, update func(*runner.SessionReservation)) bool {
	f.ensureReservations()
	updated := false
	for key, reservation := range f.reservations {
		if reservation.Allocation.Ref != ref {
			continue
		}
		update(&reservation)
		f.reservations[key] = reservation
		if f.reservation.Allocation.Ref == ref {
			f.reservation = reservation
		}
		updated = true
	}
	return updated
}

func (f *fakeDurableStore) MarkCreateSucceeded(_ context.Context, ref runner.AllocationRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createSuccess++
	f.order = append(f.order, "mark_create_succeeded")
	if f.markCreateErr != nil {
		return f.markCreateErr
	}
	if !f.updateReservation(ref, func(reservation *runner.SessionReservation) {
		if reservation.Operation.State != "succeeded" {
			reservation.Allocation.LastEventSequence++
		}
		reservation.Operation.State = "succeeded"
		reservation.Session.State = "booting"
		reservation.Allocation.ObservedState = "provisioning"
	}) {
		return runner.ErrAllocationNotFound
	}
	return nil
}

func (f *fakeDurableStore) MarkClaimedCreateSucceeded(ctx context.Context, claim runner.CreateWorkClaim) error {
	f.mu.Lock()
	current, found := f.createClaims[claim.OperationID]
	valid := found && current.LeaseToken == claim.LeaseToken
	f.mu.Unlock()
	if !valid {
		return runner.ErrLifecycleConflict
	}
	err := f.MarkCreateSucceeded(ctx, claim.Reservation.Allocation.Ref)
	f.mu.Lock()
	delete(f.createClaims, claim.OperationID)
	f.mu.Unlock()
	return err
}

func (f *fakeDurableStore) MarkReady(_ context.Context, ref runner.AllocationRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readyCalls++
	f.order = append(f.order, "ready")
	if f.markReadyErr != nil {
		return f.markReadyErr
	}
	if !f.updateReservation(ref, func(reservation *runner.SessionReservation) {
		if reservation.Session.State != "ready" {
			reservation.Allocation.LastEventSequence++
		}
		reservation.Session.State = "ready"
		reservation.Allocation.ObservedState = "running"
	}) {
		return runner.ErrAllocationNotFound
	}
	return nil
}

func (f *fakeDurableStore) MarkSettingUp(_ context.Context, ref runner.AllocationRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settingUpCalls++
	f.order = append(f.order, "setting_up")
	if !f.updateReservation(ref, func(reservation *runner.SessionReservation) {
		if reservation.Session.State != "setting_up" {
			reservation.Allocation.LastEventSequence++
		}
		reservation.Session.State = "setting_up"
	}) {
		return runner.ErrAllocationNotFound
	}
	return nil
}

func (f *fakeDurableStore) RecordVerifyFinished(_ context.Context, ref runner.AllocationRef, operationID string, success bool, verifyLog string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.verifyFinishErr != nil {
		return f.verifyFinishErr
	}
	if operationID == "" {
		return errors.New("verify operation id is required")
	}
	verifyLog = boundedVerifyLog(verifyLog)
	key, prior, found := f.findVerifyDecision(ref, operationID)
	if !found {
		return runner.ErrLifecycleConflict
	}
	if prior.Kind == runner.VerifyGradeReplay {
		if prior.Success == success && prior.Log == verifyLog {
			return nil
		}
		return runner.ErrIdempotencyConflict
	}
	if prior.Kind != runner.VerifyResume {
		return runner.ErrLifecycleConflict
	}
	valid := false
	for _, reservation := range f.reservations {
		if reservation.Allocation.Ref == ref && reservation.Session.State == "verifying" &&
			reservation.Session.DesiredState == "active" && reservation.AttemptStatus == "in_progress" {
			valid = true
			break
		}
	}
	if !valid {
		return runner.ErrLifecycleConflict
	}
	f.verifyFinishes++
	f.order = append(f.order, "verify_finished")
	if !f.updateReservation(ref, func(reservation *runner.SessionReservation) {
		reservation.Allocation.LastEventSequence++
		if success {
			reservation.Session.State = "completed"
			reservation.Session.DesiredState = "absent"
			reservation.Allocation.DesiredState = "absent"
			reservation.AttemptStatus = "success"
		} else {
			reservation.Session.State = "ready"
		}
	}) {
		return runner.ErrAllocationNotFound
	}
	if success {
		f.pendingDestroy[ref.ID] = ref
	}
	f.verifyDecisions[key] = runner.VerifyDecision{
		Kind: runner.VerifyGradeReplay, Session: ref.Session, Success: success, Log: verifyLog,
	}
	return nil
}

func (f *fakeDurableStore) RecordVerifyInfrastructureFailure(_ context.Context, ref runner.AllocationRef, operationID, code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.verifyInfraErr != nil {
		return f.verifyInfraErr
	}
	if operationID == "" {
		return errors.New("verify operation id is required")
	}
	key, prior, found := f.findVerifyDecision(ref, operationID)
	if !found {
		return runner.ErrLifecycleConflict
	}
	if prior.Kind == runner.VerifyInfrastructureReplay {
		if prior.ErrorCode == code {
			return nil
		}
		return runner.ErrIdempotencyConflict
	}
	if prior.Kind != runner.VerifyResume {
		return runner.ErrLifecycleConflict
	}
	valid := false
	for _, reservation := range f.reservations {
		if reservation.Allocation.Ref == ref && reservation.Session.State == "verifying" &&
			reservation.Session.DesiredState == "active" && reservation.AttemptStatus == "in_progress" {
			valid = true
			break
		}
	}
	if !valid {
		return runner.ErrLifecycleConflict
	}
	f.verifyInfra++
	f.order = append(f.order, "verify_infrastructure_failure")
	if !f.updateReservation(ref, func(reservation *runner.SessionReservation) {
		reservation.Allocation.LastEventSequence++
		reservation.Session.State = "ready"
	}) {
		return runner.ErrAllocationNotFound
	}
	f.verifyDecisions[key] = runner.VerifyDecision{
		Kind: runner.VerifyInfrastructureReplay, Session: ref.Session, ErrorCode: code,
	}
	return nil
}

func (f *fakeDurableStore) findVerifyDecision(ref runner.AllocationRef, operationID string) (string, runner.VerifyDecision, bool) {
	for key, decision := range f.verifyDecisions {
		if decision.Session == ref.Session && strings.HasSuffix(key, "\x00"+operationID) {
			return key, decision, true
		}
	}
	return "", runner.VerifyDecision{}, false
}

func (f *fakeDurableStore) MarkCreateFailed(_ context.Context, ref runner.AllocationRef, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureReservations()
	f.createFailures++
	f.order = append(f.order, "mark_create_failed")
	if !f.updateReservation(ref, func(reservation *runner.SessionReservation) {
		reservation.Operation.State = "cleanup_required"
		reservation.Session.State = "failed"
		reservation.Session.DesiredState = "absent"
		reservation.Allocation.DesiredState = "absent"
	}) {
		return runner.ErrAllocationNotFound
	}
	f.pendingDestroy[ref.ID] = ref
	return nil
}

func (f *fakeDurableStore) MarkClaimedCreateFailed(ctx context.Context, claim runner.CreateWorkClaim, code, message string) error {
	f.mu.Lock()
	current, found := f.createClaims[claim.OperationID]
	valid := found && current.LeaseToken == claim.LeaseToken
	f.mu.Unlock()
	if !valid {
		return runner.ErrLifecycleConflict
	}
	err := f.MarkCreateFailed(ctx, claim.Reservation.Allocation.Ref, code, message)
	f.mu.Lock()
	delete(f.createClaims, claim.OperationID)
	f.mu.Unlock()
	return err
}

func (f *fakeDurableStore) RequestDestroy(_ context.Context, ref runner.AllocationRef, _, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyRequest++
	f.order = append(f.order, "request_destroy")
	if !f.updateReservation(ref, func(reservation *runner.SessionReservation) {
		reservation.Session.DesiredState = "absent"
		reservation.Allocation.DesiredState = "absent"
	}) {
		return runner.ErrAllocationNotFound
	}
	f.pendingDestroy[ref.ID] = ref
	return nil
}

func (f *fakeDurableStore) RequestEnd(_ context.Context, userID string, expected runner.SessionRef, operationID string) (runner.EndDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureReservations()
	if expected.SessionID == "" || expected.Generation == 0 || operationID == "" {
		return runner.EndDecision{}, errors.New("end operation identity is invalid")
	}
	if f.endRequests == nil {
		f.endRequests = make(map[string]runner.SessionRef)
	}
	key := fakeVerifyKey(userID, operationID)
	if prior, found := f.endRequests[key]; found {
		if prior != expected {
			return runner.EndDecision{}, runner.ErrIdempotencyConflict
		}
	} else {
		var current *runner.SessionReservation
		for reservationKey := range f.reservations {
			candidate := f.reservations[reservationKey]
			if candidate.Session.UserID == userID && candidate.Session.ID == expected.SessionID &&
				candidate.Session.CurrentGeneration == expected.Generation && candidate.Session.DesiredState == "active" {
				copy := candidate
				current = &copy
				break
			}
		}
		if current == nil {
			return runner.EndDecision{}, runner.ErrGenerationStale
		}
		f.endRequests[key] = expected
		for reservationKey, reservation := range f.reservations {
			if reservation.Session.ID != expected.SessionID {
				continue
			}
			reservation.Session.DesiredState = "absent"
			reservation.Session.State = "failed"
			reservation.Allocation.DesiredState = "absent"
			f.reservations[reservationKey] = reservation
			f.pendingDestroy[reservation.Allocation.Ref.ID] = reservation.Allocation.Ref
		}
	}

	pending := make([]runner.AllocationRef, 0)
	for _, reservation := range f.reservations {
		if reservation.Session.ID != expected.SessionID {
			continue
		}
		if reservation.Allocation.DesiredState != "absent" {
			return runner.EndDecision{}, runner.ErrLifecycleConflict
		}
		if reservation.Allocation.ObservedState != "absent" {
			pending = append(pending, reservation.Allocation.Ref)
		}
	}
	if len(pending) == 0 {
		if f.endCommitHook != nil {
			f.endCommitHook()
		}
		return runner.EndDecision{Session: expected, State: runner.EndCompleted}, f.endReturnErr
	}
	if f.endCommitHook != nil {
		f.endCommitHook()
	}
	return runner.EndDecision{Session: expected, State: runner.EndPending, Pending: pending}, f.endReturnErr
}

func (f *fakeDurableStore) MarkDestroyed(_ context.Context, ref runner.AllocationRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyedCalls++
	f.order = append(f.order, "destroyed")
	if !f.updateReservation(ref, func(reservation *runner.SessionReservation) {
		reservation.Allocation.ObservedState = "absent"
	}) {
		return runner.ErrAllocationNotFound
	}
	delete(f.pendingDestroy, ref.ID)
	delete(f.destroyClaims, ref.ID)
	return nil
}

func (f *fakeDurableStore) ClaimDestroyWork(_ context.Context, _ string, expectedAllocationID, owner string, lease time.Duration) (runner.DestroyWorkClaim, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureReservations()
	for allocationID, ref := range f.pendingDestroy {
		if expectedAllocationID != "" && allocationID != expectedAllocationID {
			continue
		}
		if _, claimed := f.destroyClaims[allocationID]; claimed {
			continue
		}
		f.destroyAttempts[allocationID]++
		claim := runner.DestroyWorkClaim{
			OperationID:    runner.SessionIDForOperation(allocationID, "destroy"),
			LeaseToken:     runner.SessionIDForOperation(allocationID, "lease"+owner),
			LeaseOwner:     owner,
			Ref:            ref,
			Attempt:        f.destroyAttempts[allocationID],
			LeaseExpiresAt: time.Now().Add(lease),
		}
		f.destroyClaims[allocationID] = claim
		return claim, true, nil
	}
	if expectedAllocationID != "" {
		for _, reservation := range f.reservations {
			if reservation.Allocation.Ref.ID == expectedAllocationID {
				if _, claimed := f.destroyClaims[expectedAllocationID]; claimed &&
					reservation.Allocation.DesiredState == "absent" && reservation.Allocation.ObservedState != "absent" {
					return runner.DestroyWorkClaim{
						OperationID: runner.SessionIDForOperation(expectedAllocationID, "destroy"),
						Ref:         reservation.Allocation.Ref, Pending: true,
					}, true, nil
				}
			}
			if reservation.Allocation.Ref.ID == expectedAllocationID &&
				reservation.Allocation.DesiredState == "absent" && reservation.Allocation.ObservedState == "absent" {
				return runner.DestroyWorkClaim{
					OperationID: runner.SessionIDForOperation(expectedAllocationID, "destroy"),
					Ref:         reservation.Allocation.Ref, Completed: true,
				}, true, nil
			}
		}
	}
	return runner.DestroyWorkClaim{}, false, nil
}

func (f *fakeDurableStore) RetryDestroyWork(_ context.Context, claim runner.DestroyWorkClaim, _ error, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureReservations()
	current, found := f.destroyClaims[claim.Ref.ID]
	if !found || current.LeaseToken != claim.LeaseToken {
		return runner.ErrLifecycleConflict
	}
	delete(f.destroyClaims, claim.Ref.ID)
	return nil
}

func (f *fakeDurableStore) MarkClaimedDestroyed(_ context.Context, claim runner.DestroyWorkClaim) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureReservations()
	current, found := f.destroyClaims[claim.Ref.ID]
	if !found || current.LeaseToken != claim.LeaseToken {
		return runner.ErrLifecycleConflict
	}
	f.destroyedCalls++
	f.order = append(f.order, "claimed_destroyed")
	if !f.updateReservation(claim.Ref, func(reservation *runner.SessionReservation) {
		if reservation.Operation.State == "pending" || reservation.Operation.State == "running" || reservation.Operation.State == "cleanup_required" {
			reservation.Operation.State = "failed"
		}
		reservation.Allocation.ObservedState = "absent"
	}) {
		return runner.ErrAllocationNotFound
	}
	delete(f.pendingDestroy, claim.Ref.ID)
	delete(f.destroyClaims, claim.Ref.ID)
	return nil
}

func (f *fakeDurableStore) FindProvisionableReplacement(_ context.Context, destroyed runner.AllocationRef) (runner.SessionReservation, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureReservations()
	return f.findProvisionableReplacementLocked(destroyed)
}

func (f *fakeDurableStore) ClaimProvisionableReset(_ context.Context, _ string, expectedOperationID, owner string, lease time.Duration) (runner.CreateWorkClaim, bool, error) {
	return f.claimProvisionableCreate(expectedOperationID, owner, lease, true)
}

func (f *fakeDurableStore) ClaimProvisionableCreate(_ context.Context, _ string, expectedOperationID, owner string, lease time.Duration) (runner.CreateWorkClaim, bool, error) {
	return f.claimProvisionableCreate(expectedOperationID, owner, lease, false)
}

func (f *fakeDurableStore) claimProvisionableCreate(expectedOperationID, owner string, lease time.Duration, resetOnly bool) (runner.CreateWorkClaim, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureReservations()
	for key, reservation := range f.reservations {
		if (resetOnly && reservation.Allocation.Ref.Session.Generation <= 1) ||
			(reservation.Operation.State != "pending" && reservation.Operation.State != "running") ||
			(expectedOperationID != "" && reservation.Operation.ID != expectedOperationID) {
			continue
		}
		if reservation.Allocation.Ref.Session.Generation > 1 {
			oldRef := runner.AllocationRef{
				ID: runner.AllocationIDForSession(runner.SessionRef{
					SessionID: reservation.Session.ID, Generation: reservation.Allocation.Ref.Session.Generation - 1,
				}),
				Session: runner.SessionRef{
					SessionID: reservation.Session.ID, Generation: reservation.Allocation.Ref.Session.Generation - 1,
				},
				Provider: reservation.Allocation.Ref.Provider,
			}
			if _, found, _ := f.findProvisionableReplacementLocked(oldRef); !found {
				continue
			}
		}
		if _, claimed := f.createClaims[reservation.Operation.ID]; claimed {
			continue
		}
		reservation.Operation.State = "running"
		f.reservations[key] = reservation
		if f.reservation.Operation.ID == reservation.Operation.ID {
			f.reservation = reservation
		}
		claim := runner.CreateWorkClaim{
			OperationID: reservation.Operation.ID,
			LeaseToken:  runner.SessionIDForOperation(reservation.Operation.ID, "create-lease"+owner),
			LeaseOwner:  owner, Reservation: reservation, Attempt: 1,
			LeaseExpiresAt: time.Now().Add(lease),
		}
		f.createClaims[reservation.Operation.ID] = claim
		return claim, true, nil
	}
	return runner.CreateWorkClaim{}, false, nil
}

func (f *fakeDurableStore) findProvisionableReplacementLocked(destroyed runner.AllocationRef) (runner.SessionReservation, bool, error) {
	oldAbsent := false
	for _, reservation := range f.reservations {
		if reservation.Allocation.Ref == destroyed {
			oldAbsent = reservation.Allocation.DesiredState == "absent" && reservation.Allocation.ObservedState == "absent"
			break
		}
	}
	if !oldAbsent {
		return runner.SessionReservation{}, false, nil
	}
	for _, reservation := range f.reservations {
		if reservation.Session.ID == destroyed.Session.SessionID &&
			reservation.Allocation.Ref.Session.Generation == destroyed.Session.Generation+1 &&
			reservation.Operation.State == "pending" {
			return reservation, true, nil
		}
	}
	return runner.SessionReservation{}, false, nil
}

func newDurableTestService(t *testing.T) (*Service, *mockRunner, *mockProblemStore, *fakeDurableStore) {
	t.Helper()
	runtime := &mockRunner{running: true, setupGates: make(map[uint64]chan struct{})}
	problems := newMockProblemStore()
	problems.problems["p1"] = &models.Problem{
		ID: "p1", Revision: "revision-p1", TimeoutMinutes: 30, VerifyType: "script",
	}
	durable := &fakeDurableStore{}
	svc, err := NewDurablePausedService(runtime, problems, problems, durable, "local-docker:test")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Activate(); err != nil {
		t.Fatal(err)
	}
	svc.SetVerifyThrottle(0)
	t.Cleanup(func() {
		svc.BeginShutdown()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = svc.WaitForWatchers(ctx)
	})
	return svc, runtime, problems, durable
}

func startDurableReadySession(t *testing.T, svc *Service, userID string) *CurrentSession {
	t.Helper()
	snapshot, err := svc.StartProblemOperation(
		context.Background(), userID, "p1", "start:p1:verify-tests-"+userID,
	)
	if err != nil {
		t.Fatalf("start durable verify session: %v", err)
	}
	waitForStatus(t, svc, userID, StatusReady)
	svc.setupWG.Wait()
	return snapshot
}

func TestDurableEndCommitImmediatelyFencesExactLocalTerminal(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	startDurableReadySession(t, svc, "user-1")
	sess := svc.GetSession("user-1")
	sess.mu.Lock()
	ref := sess.Allocation
	sess.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	durable.mu.Lock()
	var cancelOnce sync.Once
	durable.endCommitHook = func() { cancelOnce.Do(cancel) }
	durable.mu.Unlock()
	decision, err := svc.EndSessionOperation(ctx, "user-1", ref.Session, "end:fence-local-terminal-0001")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("end decision=%+v err=%v, want cancelled after durable commit", decision, err)
	}
	if current := svc.GetSession("user-1"); current == nil {
		t.Fatal("pending cleanup evicted the exact local session")
	}
	if _, ok := svc.GetTerminalTarget("user-1", ref.Session); ok {
		t.Fatal("durable end commit left an exact local terminal target")
	}
	if release, ok := svc.AcquireTerminalCommit(context.Background(), "user-1", ref); ok || release != nil {
		t.Fatal("durable end commit allowed an exact terminal ownership commit")
	}

	runtime.mu.Lock()
	destroys := len(runtime.removed)
	runtime.mu.Unlock()
	if destroys != 0 {
		t.Fatalf("cancelled end request mutated provider %d time(s)", destroys)
	}
}

func TestDurableMarkReadyFailureDoesNotDeadlockCleanup(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	durable.markReadyErr = errors.New("mark ready failed")
	snapshot, err := svc.StartProblemOperation(context.Background(), "user-1", "p1", "start:mark-ready-failure-0001")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() { svc.setupWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("MarkReady failure deadlocked setup cleanup")
	}
	runtime.mu.Lock()
	removed := append([]runner.AllocationRef(nil), runtime.removed...)
	runtime.mu.Unlock()
	wantRef := runner.AllocationRef{
		ID:       runner.AllocationIDForSession(runner.SessionRef{SessionID: snapshot.SessionID, Generation: snapshot.Generation}),
		Session:  runner.SessionRef{SessionID: snapshot.SessionID, Generation: snapshot.Generation},
		Provider: runner.ProviderLocalDocker,
	}
	if len(removed) != 1 || removed[0] != wantRef {
		t.Fatalf("MarkReady failure cleanup removed=%+v, want exact %+v", removed, wantRef)
	}
	if svc.GetSession("user-1") != nil {
		t.Fatal("MarkReady failure cleanup retained the failed local session")
	}
	lock := svc.userLock("user-1")
	if !lock.TryLock() {
		t.Fatal("MarkReady failure did not return the user transition lock")
	}
	lock.Unlock()
}

func TestDurableUnknownEndCommitImmediatelyFencesExactLocalTerminal(t *testing.T) {
	svc, _, _, durable := newDurableTestService(t)
	startDurableReadySession(t, svc, "user-1")
	sess := svc.GetSession("user-1")
	sess.mu.Lock()
	ref := sess.Allocation
	sess.mu.Unlock()
	durable.endReturnErr = runner.ErrOperationOutcomeUnknown

	if _, err := svc.EndSessionOperation(context.Background(), "user-1", ref.Session, "end:unknown-fence-0001"); !errors.Is(err, runner.ErrOperationOutcomeUnknown) {
		t.Fatalf("unknown end commit error=%v", err)
	}
	if _, ok := svc.GetTerminalTarget("user-1", ref.Session); ok {
		t.Fatal("unknown durable end outcome left an exact local terminal target")
	}
}

func TestDurableEndWaitsForTerminalCommitLease(t *testing.T) {
	svc, _, _, _ := newDurableTestService(t)
	startDurableReadySession(t, svc, "user-1")
	sess := svc.GetSession("user-1")
	sess.mu.Lock()
	ref := sess.Allocation
	sess.mu.Unlock()

	release, ok := svc.AcquireTerminalCommit(context.Background(), "user-1", ref)
	if !ok || release == nil {
		t.Fatal("ready allocation did not acquire terminal commit lease")
	}
	endDone := make(chan error, 1)
	go func() {
		_, err := svc.EndSessionOperation(context.Background(), "user-1", ref.Session, "end:wait-terminal-commit-0001")
		endDone <- err
	}()
	select {
	case err := <-endDone:
		t.Fatalf("end crossed held terminal commit lease: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case err := <-endDone:
		if err != nil {
			t.Fatalf("end after terminal commit release: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("end did not proceed after terminal commit release")
	}
}

func TestDurableDestroyWorkerWaitsForTerminalCommitLease(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	startDurableReadySession(t, svc, "user-1")
	sess := svc.GetSession("user-1")
	sess.mu.Lock()
	ref := sess.Allocation
	sess.mu.Unlock()

	release, ok := svc.AcquireTerminalCommit(context.Background(), "user-1", ref)
	if !ok || release == nil {
		t.Fatal("ready allocation did not acquire terminal commit lease")
	}
	if _, err := durable.RequestEnd(context.Background(), "user-1", ref.Session, "end:worker-waits-terminal-0001"); err != nil {
		release()
		t.Fatal(err)
	}
	runtime.destroyEntered = make(chan runner.AllocationRef, 1)
	workerDone := make(chan error, 1)
	go func() {
		_, err := svc.processDestroyWorkOnce(context.Background(), "worker-terminal-linearization")
		workerDone <- err
	}()
	select {
	case got := <-runtime.destroyEntered:
		release()
		t.Fatalf("destroy worker crossed terminal commit lease for %+v", got)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case got := <-runtime.destroyEntered:
		if got != ref {
			t.Fatalf("destroy worker targeted %+v, want %+v", got, ref)
		}
	case <-time.After(time.Second):
		t.Fatal("destroy worker did not proceed after terminal commit release")
	}
	select {
	case err := <-workerDone:
		if err != nil {
			t.Fatalf("destroy worker after terminal commit release: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("destroy worker did not finish")
	}
	if current := svc.GetSession("user-1"); current != nil {
		t.Fatalf("destroyed exact local allocation remained current: %+v", current)
	}
}

func evictVerifyOperationCache(svc *Service, userID, operationID string) {
	svc.mu.Lock()
	delete(svc.verifyOps, fakeVerifyKey(userID, operationID))
	svc.mu.Unlock()
}

func countRuntimeVerifies(runtime *mockRunner) int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return len(runtime.verifyKeys)
}

func makeDurableChoiceSession(t *testing.T) (*Service, *mockRunner, *fakeDurableStore, runner.SessionRef) {
	t.Helper()
	svc, runtime, problems, durable := newDurableTestService(t)
	problems.mu.Lock()
	choice := *problems.problems["p1"]
	choice.VerifyType = "choice"
	choice.CorrectChoice = "b"
	problems.problems["p1"] = &choice
	problems.mu.Unlock()
	startDurableReadySession(t, svc, "user-1")
	current := svc.GetCurrentSession("user-1")
	if current == nil {
		t.Fatal("durable choice session is missing")
	}
	return svc, runtime, durable, runner.SessionRef{SessionID: current.SessionID, Generation: current.Generation}
}

func TestDurableChoiceExactReplaySurvivesSessionRemovalWithoutRegrading(t *testing.T) {
	svc, runtime, durable, expected := makeDurableChoiceSession(t)
	const operationID = "choice-durable-replay-0001"

	success, err := svc.SubmitChoice(context.Background(), "user-1", "p1", expected, operationID, "b")
	if err != nil || !success {
		t.Fatalf("first durable choice success=%v err=%v", success, err)
	}
	if current := svc.GetSession("user-1"); current != nil {
		t.Fatalf("successful durable choice retained local session: %+v", current)
	}
	success, err = svc.SubmitChoice(context.Background(), "user-1", "p1", expected, operationID, "b")
	if err != nil || !success {
		t.Fatalf("durable choice replay success=%v err=%v", success, err)
	}
	if calls := countRuntimeVerifies(runtime); calls != 0 {
		t.Fatalf("choice grading reached Runner %d times", calls)
	}
	durable.mu.Lock()
	starts, finishes := durable.verifyStarts, durable.verifyFinishes
	durable.mu.Unlock()
	if starts != 1 || finishes != 1 {
		t.Fatalf("choice replay mutated durable grade starts=%d finishes=%d", starts, finishes)
	}
}

func TestDurableChoiceOperationBindsAnswerProblemAndGeneration(t *testing.T) {
	svc, _, _, expected := makeDurableChoiceSession(t)
	const operationID = "choice-durable-binding-0001"
	if success, err := svc.SubmitChoice(context.Background(), "user-1", "p1", expected, operationID, "a"); err != nil || success {
		t.Fatalf("initial incorrect choice success=%v err=%v", success, err)
	}

	for name, test := range map[string]struct {
		problemID string
		session   runner.SessionRef
		choiceID  string
	}{
		"answer":     {problemID: "p1", session: expected, choiceID: "b"},
		"problem":    {problemID: "another-problem", session: expected, choiceID: "a"},
		"generation": {problemID: "p1", session: runner.SessionRef{SessionID: expected.SessionID, Generation: expected.Generation + 1}, choiceID: "a"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.SubmitChoice(context.Background(), "user-1", test.problemID, test.session, operationID, test.choiceID); !errors.Is(err, runner.ErrIdempotencyConflict) {
				t.Fatalf("changed %s error=%v, want idempotency conflict", name, err)
			}
		})
	}
}

func TestDurableChoicePersistenceFailureDoesNotPublishSuccess(t *testing.T) {
	svc, _, durable, expected := makeDurableChoiceSession(t)
	databaseErr := errors.New("choice transaction failed")
	durable.choiceRecordErr = databaseErr
	verifyCallbacks, endedCallbacks := 0, 0
	svc.SetVerifyCallback(func(string, runner.SessionRef, bool, string) { verifyCallbacks++ })
	svc.SetEndedCallback(func(string, runner.SessionRef, string) { endedCallbacks++ })

	if _, err := svc.SubmitChoice(context.Background(), "user-1", "p1", expected, "choice-durable-failure-0001", "b"); !errors.Is(err, databaseErr) {
		t.Fatalf("choice persistence error=%v, want database failure", err)
	}
	current := svc.GetCurrentSession("user-1")
	if current == nil || current.Status != StatusReady {
		t.Fatalf("failed choice transaction changed local state: %+v", current)
	}
	if verifyCallbacks != 0 || endedCallbacks != 0 {
		t.Fatalf("failed choice transaction published callbacks verify=%d ended=%d", verifyCallbacks, endedCallbacks)
	}
}

func TestDurableVerifyBeginFailureDoesNotPublishOrThrottle(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	startDurableReadySession(t, svc, "user-1")
	svc.SetVerifyThrottle(time.Hour)
	databaseErr := errors.New("database unavailable")
	durable.beginVerifyErr = databaseErr

	const operationID = "verify-begin-failure-0001"
	if _, _, err := svc.Verify(context.Background(), "user-1", operationID); !errors.Is(err, databaseErr) {
		t.Fatalf("begin verify error = %v, want database failure", err)
	}
	if calls := countRuntimeVerifies(runtime); calls != 0 {
		t.Fatalf("failed durable admission reached Runner %d times", calls)
	}
	if current := svc.GetCurrentSession("user-1"); current == nil || current.Status != StatusReady {
		t.Fatalf("failed durable admission changed local state: %+v", current)
	}

	// A failed DB admission did not start an expensive verify and therefore
	// must not consume the per-user verify throttle.
	durable.beginVerifyErr = nil
	runtime.verifyResult = runner.VerifyResult{Status: runner.VerifyFailed}
	if success, _, err := svc.Verify(context.Background(), "user-1", operationID); err != nil || success {
		t.Fatalf("retry after admission failure success=%v err=%v", success, err)
	}
	if calls := countRuntimeVerifies(runtime); calls != 1 {
		t.Fatalf("admitted retry reached Runner %d times, want 1", calls)
	}
}

func TestDurableVerifyGradeReplaySurvivesProcessCacheEviction(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	startDurableReadySession(t, svc, "user-1")
	runtime.verifyResult = runner.VerifyResult{Status: runner.VerifyFailed}
	const operationID = "verify-grade-replay-0001"

	firstSuccess, firstLog, err := svc.Verify(context.Background(), "user-1", operationID)
	if err != nil || firstSuccess {
		t.Fatalf("first durable grade success=%v log=%q err=%v", firstSuccess, firstLog, err)
	}
	evictVerifyOperationCache(svc, "user-1", operationID)
	svc.SetVerifyThrottle(time.Hour)

	secondSuccess, secondLog, err := svc.Verify(context.Background(), "user-1", operationID)
	if err != nil || secondSuccess || secondLog != firstLog {
		t.Fatalf("durable grade replay success=%v log=%q err=%v", secondSuccess, secondLog, err)
	}
	if calls := countRuntimeVerifies(runtime); calls != 1 {
		t.Fatalf("durable grade replay reached Runner %d times, want 1", calls)
	}
	durable.mu.Lock()
	starts, finishes := durable.verifyStarts, durable.verifyFinishes
	durable.mu.Unlock()
	if starts != 1 || finishes != 1 {
		t.Fatalf("durable grade mutations starts=%d finishes=%d, want 1/1", starts, finishes)
	}
}

func TestDurableVerifyRunningOperationIsBusyAndNeverReexecutesProvider(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	startDurableReadySession(t, svc, "user-1")
	sess := svc.GetSession("user-1")
	if sess == nil {
		t.Fatal("ready durable session is missing")
	}
	sess.mu.Lock()
	ref := sess.Allocation
	sess.mu.Unlock()
	const operationID = "verify-running-replay-0001"
	decision, err := durable.BeginVerify(context.Background(), "user-1", ref, operationID)
	if err != nil || decision.Kind != runner.VerifyExecute {
		t.Fatalf("seed running verify decision=%+v err=%v", decision, err)
	}

	if _, _, err := svc.Verify(context.Background(), "user-1", operationID); !errors.Is(err, ErrTransitionBusy) {
		t.Fatalf("running durable verify error=%v, want transition busy", err)
	}
	if calls := countRuntimeVerifies(runtime); calls != 0 {
		t.Fatalf("running durable verify reexecuted provider %d times", calls)
	}
}

func TestDurableVerifyCacheReplayRejectsNewerReservedSession(t *testing.T) {
	svc, runtime, _, _ := newDurableTestService(t)
	startDurableReadySession(t, svc, "user-1")
	runtime.verifyResult = runner.VerifyResult{Status: runner.VerifyPassed}
	const verifyOperation = "verify-old-session-cache-0001"
	if success, _, err := svc.Verify(context.Background(), "user-1", verifyOperation); err != nil || !success {
		t.Fatalf("complete old session success=%v err=%v", success, err)
	}
	svc.SetStartCooldown(0)

	runtime.createEntered = make(chan runner.CreateSessionRequest, 1)
	runtime.createGate = make(chan struct{})
	type startResult struct {
		snapshot *CurrentSession
		err      error
	}
	started := make(chan startResult, 1)
	go func() {
		snapshot, err := svc.StartProblemOperation(
			context.Background(), "user-1", "p1", "start:p1:newer-reserved-session",
		)
		started <- startResult{snapshot: snapshot, err: err}
	}()

	select {
	case <-runtime.createEntered:
		// ReserveSession and the create claim have committed, but the new
		// process-local Session is intentionally not published yet.
	case <-time.After(time.Second):
		t.Fatal("new session did not reach provider create")
	}
	if _, _, err := svc.Verify(context.Background(), "user-1", verifyOperation); !errors.Is(err, runner.ErrIdempotencyConflict) {
		t.Fatalf("old cached verify during newer reservation error=%v, want idempotency conflict", err)
	}
	if calls := countRuntimeVerifies(runtime); calls != 1 {
		t.Fatalf("old cached verify reached Runner %d times, want 1", calls)
	}

	close(runtime.createGate)
	result := <-started
	if result.err != nil || result.snapshot == nil {
		t.Fatalf("new session start snapshot=%+v err=%v", result.snapshot, result.err)
	}
	waitForStatus(t, svc, "user-1", StatusReady)
}

func TestDurableVerifyInfrastructureReplayRequiresNewOperationForRetry(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	startDurableReadySession(t, svc, "user-1")
	runtime.verifyErr = errors.New("provider unavailable")
	const operationID = "verify-infrastructure-replay-0001"

	if _, _, err := svc.Verify(context.Background(), "user-1", operationID); !errors.Is(err, ErrVerifyInfrastructure) {
		t.Fatalf("first infrastructure verify error = %v", err)
	}
	evictVerifyOperationCache(svc, "user-1", operationID)
	runtime.mu.Lock()
	runtime.verifyErr = nil
	runtime.verifyResult = runner.VerifyResult{Status: runner.VerifyFailed}
	runtime.mu.Unlock()

	if _, _, err := svc.Verify(context.Background(), "user-1", operationID); !errors.Is(err, ErrVerifyInfrastructure) {
		t.Fatalf("same infrastructure operation replay error = %v", err)
	}
	if calls := countRuntimeVerifies(runtime); calls != 1 {
		t.Fatalf("same infrastructure operation re-executed Runner: calls=%d", calls)
	}

	svc.SetVerifyThrottle(0)
	if success, log, err := svc.Verify(context.Background(), "user-1", "verify-infrastructure-new-0002"); err != nil || success || log != "Verification did not pass." {
		t.Fatalf("new infrastructure operation success=%v log=%q err=%v", success, log, err)
	}
	if calls := countRuntimeVerifies(runtime); calls != 2 {
		t.Fatalf("new infrastructure operation Runner calls=%d, want 2", calls)
	}
	durable.mu.Lock()
	infraResults := durable.verifyInfra
	durable.mu.Unlock()
	if infraResults != 1 {
		t.Fatalf("infrastructure terminal results=%d, want 1", infraResults)
	}
}

func TestDurableVerifyRejectsMismatchedReceiptAsInfrastructureFailure(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	startDurableReadySession(t, svc, "user-1")
	runtime.verifyResult = runner.VerifyResult{
		Status:   runner.VerifyFailed,
		Evidence: "SECRET_TOKEN=receipt-mismatch-must-stay-private",
	}
	runtime.verifyResultMutator = func(result *runner.VerifyResult) {
		result.Receipt.Problem.Revision = "attacker-selected-revision"
	}
	callbackCalls := 0
	svc.SetVerifyCallback(func(string, runner.SessionRef, bool, string) { callbackCalls++ })

	const operationID = "verify-receipt-mismatch-0001"
	if _, logValue, err := svc.Verify(context.Background(), "user-1", operationID); !errors.Is(err, ErrVerifyInfrastructure) || logValue != "" {
		t.Fatalf("mismatched receipt result log=%q err=%v, want safe infrastructure failure", logValue, err)
	}
	if callbackCalls != 0 {
		t.Fatalf("mismatched receipt emitted %d grade callbacks", callbackCalls)
	}
	if current := svc.GetCurrentSession("user-1"); current == nil || current.Status != StatusReady {
		t.Fatalf("mismatched receipt did not restore ready state: %+v", current)
	}
	durable.mu.Lock()
	defer durable.mu.Unlock()
	if durable.verifyFinishes != 0 || durable.verifyInfra != 1 {
		t.Fatalf("mismatched receipt durable terminals grade=%d infra=%d, want 0/1", durable.verifyFinishes, durable.verifyInfra)
	}
	_, decision, found := durable.findVerifyDecision(runtime.verifyRequests[0].Allocation, operationID)
	if !found || decision.Kind != runner.VerifyInfrastructureReplay || decision.ErrorCode != "verification_infrastructure_error" {
		t.Fatalf("mismatched receipt durable decision = %+v found=%v", decision, found)
	}
	if strings.Contains(decision.Log, "SECRET_TOKEN") {
		t.Fatalf("private evidence leaked into infrastructure decision: %q", decision.Log)
	}
}

func TestDurableVerifyResultPersistenceFailureKeepsLocalStateUncommitted(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	startDurableReadySession(t, svc, "user-1")
	runtime.verifyResult = runner.VerifyResult{Status: runner.VerifyFailed}
	durable.verifyFinishErr = errors.New("result commit failed")
	callbackCalls := 0
	svc.SetVerifyCallback(func(string, runner.SessionRef, bool, string) { callbackCalls++ })

	if _, _, err := svc.Verify(context.Background(), "user-1", "verify-result-commit-0001"); !errors.Is(err, durable.verifyFinishErr) {
		t.Fatalf("verify result persistence error = %v", err)
	}
	if current := svc.GetCurrentSession("user-1"); current == nil || current.Status != StatusVerifying {
		t.Fatalf("uncommitted grade was published locally: %+v", current)
	}
	if callbackCalls != 0 {
		t.Fatalf("uncommitted grade emitted %d callbacks", callbackCalls)
	}
}

func TestDurableVerifyInfrastructurePersistenceFailureKeepsLocalStateUncommitted(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	startDurableReadySession(t, svc, "user-1")
	runtime.verifyErr = errors.New("provider unavailable")
	durable.verifyInfraErr = errors.New("infrastructure result commit failed")

	_, _, err := svc.Verify(context.Background(), "user-1", "verify-infra-commit-0001")
	if !errors.Is(err, runtime.verifyErr) || !errors.Is(err, durable.verifyInfraErr) {
		t.Fatalf("infrastructure persistence error = %v", err)
	}
	if errors.Is(err, ErrVerifyInfrastructure) {
		t.Fatalf("uncommitted infrastructure result was reported as replayable: %v", err)
	}
	if current := svc.GetCurrentSession("user-1"); current == nil || current.Status != StatusVerifying {
		t.Fatalf("uncommitted infrastructure result was published locally: %+v", current)
	}
}

func TestDurableVerifyPublishesOnlyAfterPersistenceAndCleanupIntent(t *testing.T) {
	t.Run("failed grade before callback", func(t *testing.T) {
		svc, runtime, _, durable := newDurableTestService(t)
		startDurableReadySession(t, svc, "user-1")
		runtime.verifyResult = runner.VerifyResult{Status: runner.VerifyFailed}
		svc.SetVerifyCallback(func(string, runner.SessionRef, bool, string) {
			durable.mu.Lock()
			durable.order = append(durable.order, "verify_callback")
			durable.mu.Unlock()
		})

		if _, _, err := svc.Verify(context.Background(), "user-1", "verify-order-failed-0001"); err != nil {
			t.Fatal(err)
		}
		durable.mu.Lock()
		order := append([]string(nil), durable.order...)
		durable.mu.Unlock()
		finished := indexOfString(order, "verify_finished")
		callback := indexOfString(order, "verify_callback")
		if finished < 0 || callback <= finished {
			t.Fatalf("failed-grade publication order = %v", order)
		}
	})

	t.Run("passed grade intent before provider destroy", func(t *testing.T) {
		svc, runtime, _, durable := newDurableTestService(t)
		startDurableReadySession(t, svc, "user-1")
		runtime.verifyResult = runner.VerifyResult{Status: runner.VerifyPassed}

		if success, _, err := svc.Verify(context.Background(), "user-1", "verify-order-passed-0001"); err != nil || !success {
			t.Fatalf("passed verify success=%v err=%v", success, err)
		}
		durable.mu.Lock()
		order := append([]string(nil), durable.order...)
		durable.mu.Unlock()
		finished := indexOfString(order, "verify_finished")
		destroyed := indexOfString(order, "claimed_destroyed")
		if finished < 0 || destroyed <= finished {
			t.Fatalf("passed-grade cleanup order = %v", order)
		}
	})
}

func TestDurableVerifyDoesNotPublishOrPersistPrivateEvidence(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	startDurableReadySession(t, svc, "user-1")
	const privateEvidence = "SECRET_TOKEN=do-not-publish\n10.0.0.1\n-----BEGIN PRIVATE KEY-----"
	runtime.verifyResult = runner.VerifyResult{Status: runner.VerifyFailed, Evidence: privateEvidence}

	_, logValue, err := svc.Verify(context.Background(), "user-1", "verify-private-evidence-0001")
	if err != nil {
		t.Fatal(err)
	}
	if logValue != "Verification did not pass." || strings.Contains(logValue, privateEvidence) {
		t.Fatalf("private evidence leaked through verify response: %q", logValue)
	}
	durable.mu.Lock()
	defer durable.mu.Unlock()
	for _, decision := range durable.verifyDecisions {
		if strings.Contains(decision.Log, privateEvidence) || strings.Contains(decision.Log, "SECRET_TOKEN") || strings.Contains(decision.Log, "PRIVATE KEY") {
			t.Fatalf("private evidence leaked into durable verify result: %q", decision.Log)
		}
	}
}

func indexOfString(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}

func newDurableRecoveryTestService(t *testing.T) (*Service, *mockRunner, *mockProblemStore, *fakeDurableStore, *runner.AuthorityGate) {
	t.Helper()
	runtime := &mockRunner{running: true, setupGates: make(map[uint64]chan struct{})}
	problems := newMockProblemStore()
	problems.problems["p1"] = &models.Problem{
		ID: "p1", Revision: "revision-p1", TimeoutMinutes: 30, VerifyType: "script",
	}
	durable := &fakeDurableStore{}
	gate := runner.NewAuthorityGate()
	authorized, err := runner.NewAuthorityRunner(runtime, gate, runner.ControllerFence{
		ProviderID: "local-docker:test", Epoch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewDurablePausedService(authorized, problems, problems, durable, "local-docker:test")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetAuthorityGate(gate); err != nil {
		t.Fatal(err)
	}
	return svc, runtime, problems, durable, gate
}

func reserveRecoverySession(t *testing.T, durable *fakeDurableStore, key string) runner.SessionReservation {
	t.Helper()
	reservation, err := durable.ReserveSession(context.Background(), runner.ReserveSessionParams{
		SessionID: "55555555-5555-4555-8555-555555555555", UserID: "user-1",
		Selection: runner.CatalogSelection{Generation: 1, Problem: runner.ProblemRef{ID: "p1", Revision: "revision-p1"}},
		Provider:  runner.ProviderLocalDocker, ProviderID: "local-docker:test",
		ResourceProfile: runner.DefaultResourceProfile, ExpiresAt: time.Now().Add(time.Hour),
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return reservation
}

func TestDurableRecoveryResumesGenerationOneWithoutHTTPRetry(t *testing.T) {
	svc, runtime, _, durable, gate := newDurableRecoveryTestService(t)
	reservation := reserveRecoverySession(t, durable, "start:recovery-generation-one")

	if err := svc.RecoverDurableState(context.Background()); err != nil {
		t.Fatalf("recover generation one: %v", err)
	}
	current := svc.GetCurrentSession("user-1")
	if current == nil || current.SessionID != reservation.Session.ID || current.Generation != 1 || current.Status != StatusReady {
		t.Fatalf("recovered current session = %+v", current)
	}
	runtime.mu.Lock()
	created := append([]runner.CreateSessionRequest(nil), runtime.created...)
	runtime.mu.Unlock()
	if len(created) != 1 || created[0].AllocationID != reservation.Allocation.Ref.ID || created[0].IdempotencyKey != reservation.Operation.IdempotencyKey {
		t.Fatalf("recovery create calls = %+v", created)
	}
	if gate.State() != runner.AuthorityRecovering || svc.IsAccepting() {
		t.Fatalf("recovery opened admission early: gate=%s accepting=%v", gate.State(), svc.IsAccepting())
	}
}

func TestDurableRecoveryAdoptsReadyAllocationWithoutCreateOrSetup(t *testing.T) {
	svc, runtime, _, durable, _ := newDurableRecoveryTestService(t)
	reservation := reserveRecoverySession(t, durable, "start:recovery-ready")
	if err := durable.MarkCreateSucceeded(context.Background(), reservation.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	if err := durable.MarkReady(context.Background(), reservation.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	durable.mu.Lock()
	readyCallsBefore := durable.readyCalls
	durable.mu.Unlock()

	if err := svc.RecoverDurableState(context.Background()); err != nil {
		t.Fatalf("adopt ready allocation: %v", err)
	}
	current := svc.GetCurrentSession("user-1")
	if current == nil || current.Status != StatusReady || current.Generation != 1 {
		t.Fatalf("adopted current session = %+v", current)
	}
	runtime.mu.Lock()
	creates := len(runtime.created)
	runtime.mu.Unlock()
	durable.mu.Lock()
	readyCallsAfter := durable.readyCalls
	durable.mu.Unlock()
	if creates != 0 || readyCallsAfter != readyCallsBefore {
		t.Fatalf("ready adoption repeated create/setup persistence: creates=%d ready=%d/%d", creates, readyCallsBefore, readyCallsAfter)
	}
}

func TestDurableRecoveryReplaysSetupKeyWithoutRepeatingProviderEffect(t *testing.T) {
	svc, runtime, _, durable, _ := newDurableRecoveryTestService(t)
	reservation := reserveRecoverySession(t, durable, "start:recovery-setup-commit-window")
	if err := durable.MarkCreateSucceeded(context.Background(), reservation.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	setupRequest := runner.SetupSessionRequest{
		Allocation:     reservation.Allocation.Ref,
		IdempotencyKey: runner.SetupIdempotencyKey(reservation.Allocation.Ref),
	}
	// Simulate process A completing the provider/guest setup effect and then
	// crashing before PostgreSQL records the ready transition.
	if err := runtime.SetupSession(context.Background(), setupRequest); err != nil {
		t.Fatalf("process A setup: %v", err)
	}

	if err := svc.RecoverDurableState(context.Background()); err != nil {
		t.Fatalf("recover setup commit window: %v", err)
	}
	current := svc.GetCurrentSession("user-1")
	if current == nil || current.Status != StatusReady || current.Generation != 1 {
		t.Fatalf("recovered setup commit-window session = %+v", current)
	}
	runtime.mu.Lock()
	requests := append([]runner.SetupSessionRequest(nil), runtime.setupRequests...)
	effects := runtime.setupEffects
	runtime.mu.Unlock()
	if len(requests) != 2 || requests[0] != setupRequest || requests[1] != setupRequest {
		t.Fatalf("setup retry identity changed across restart: %+v", requests)
	}
	if effects != 1 {
		t.Fatalf("setup provider effect ran %d times across restart, want 1", effects)
	}
	durable.mu.Lock()
	readyCalls := durable.readyCalls
	durable.mu.Unlock()
	if readyCalls != 1 {
		t.Fatalf("ready transition count = %d, want 1", readyCalls)
	}
}

func TestDurableRecoveryDestroysResetSourceThenCreatesGenerationTwo(t *testing.T) {
	svc, runtime, _, durable, _ := newDurableRecoveryTestService(t)
	start := reserveRecoverySession(t, durable, "start:recovery-reset")
	if err := durable.MarkCreateSucceeded(context.Background(), start.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	if err := durable.MarkReady(context.Background(), start.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	reset, err := durable.ReserveReset(context.Background(), runner.ReserveResetParams{
		Expected: start.Allocation.Ref.Session, UserID: "user-1", Provider: runner.ProviderLocalDocker,
		ProviderID: "local-docker:test", IdempotencyKey: "reset:recovery-generation-two",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.RecoverDurableState(context.Background()); err != nil {
		t.Fatalf("recover reset replacement: %v", err)
	}
	current := svc.GetCurrentSession("user-1")
	if current == nil || current.Generation != 2 || current.SessionID != start.Session.ID || current.Status != StatusReady {
		t.Fatalf("recovered reset current = %+v", current)
	}
	runtime.mu.Lock()
	created := append([]runner.CreateSessionRequest(nil), runtime.created...)
	removed := append([]runner.AllocationRef(nil), runtime.removed...)
	runtime.mu.Unlock()
	if len(removed) != 1 || removed[0] != reset.Old || len(created) != 1 || created[0].Session.Generation != 2 {
		t.Fatalf("reset recovery provider calls removed=%+v created=%+v", removed, created)
	}
}

func TestDurableRecoveryExpiresAndEvictsReadyAllocation(t *testing.T) {
	svc, runtime, _, durable, _ := newDurableRecoveryTestService(t)
	reservation := reserveRecoverySession(t, durable, "start:recovery-expired")
	if err := durable.MarkCreateSucceeded(context.Background(), reservation.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	if err := durable.MarkReady(context.Background(), reservation.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	durable.mu.Lock()
	durable.updateReservation(reservation.Allocation.Ref, func(stored *runner.SessionReservation) {
		stored.Session.ExpiresAt = time.Now().Add(-time.Minute)
		stored.Allocation.ExpiresAt = time.Now().Add(-time.Minute)
	})
	durable.mu.Unlock()

	if err := svc.RecoverDurableState(context.Background()); err != nil {
		t.Fatalf("recover expired allocation: %v", err)
	}
	if current := svc.GetCurrentSession("user-1"); current != nil {
		t.Fatalf("expired recovery retained current session: %+v", current)
	}
	runtime.mu.Lock()
	removed := append([]runner.AllocationRef(nil), runtime.removed...)
	runtime.mu.Unlock()
	if len(removed) != 1 || removed[0] != reservation.Allocation.Ref {
		t.Fatalf("expired recovery cleanup = %+v", removed)
	}
}

func TestDurableStartReservesBeforeProviderAndReturnsOperationSnapshot(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	runtime.createEntered = make(chan runner.CreateSessionRequest, 1)
	runtime.createGate = make(chan struct{})

	type result struct {
		snapshot *CurrentSession
		err      error
	}
	done := make(chan result, 1)
	go func() {
		snapshot, err := svc.StartProblemOperation(context.Background(), "user-1", "p1", "start:p1:operation-12345678")
		done <- result{snapshot: snapshot, err: err}
	}()

	select {
	case request := <-runtime.createEntered:
		durable.mu.Lock()
		reserveCalls := durable.reserveCalls
		order := append([]string(nil), durable.order...)
		durable.mu.Unlock()
		if reserveCalls != 1 || len(order) < 2 || order[len(order)-1] != "reserve" {
			t.Fatalf("provider reached before durable reservation: calls=%d order=%v", reserveCalls, order)
		}
		if request.AllocationID != runner.AllocationIDForSession(request.Session) || request.IdempotencyKey == "" {
			t.Fatalf("unsafe provider request: %+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("provider create was not called")
	}
	close(runtime.createGate)

	started := <-done
	if started.err != nil {
		t.Fatalf("durable start: %v", started.err)
	}
	if started.snapshot == nil || started.snapshot.OperationID == "" || started.snapshot.Generation != 1 || started.snapshot.ProblemID != "p1" {
		t.Fatalf("operation-bound snapshot = %+v", started.snapshot)
	}
	durable.mu.Lock()
	if durable.createSuccess != 1 {
		t.Fatalf("create success transitions = %d, want 1", durable.createSuccess)
	}
	durable.mu.Unlock()
	waitForStatus(t, svc, "user-1", StatusReady)
}

func TestDurableStartRejectsSameUserWaitQueueWhileTransitionRuns(t *testing.T) {
	svc, runtime, _, _ := newDurableTestService(t)
	runtime.createEntered = make(chan runner.CreateSessionRequest, 1)
	runtime.createGate = make(chan struct{})

	type result struct {
		snapshot *CurrentSession
		err      error
	}
	firstDone := make(chan result, 1)
	go func() {
		snapshot, err := svc.StartProblemOperation(context.Background(), "user-1", "p1", "start:p1:queue-holder-12345678")
		firstDone <- result{snapshot: snapshot, err: err}
	}()

	select {
	case <-runtime.createEntered:
	case <-time.After(time.Second):
		t.Fatal("first transition did not reach the provider")
	}

	const attempts = 100
	results := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		go func(index int) {
			_, err := svc.StartProblemOperation(
				context.Background(), "user-1", "p1", fmt.Sprintf("start:p1:queued-%08d", index),
			)
			results <- err
		}(i)
	}
	deadline := time.After(time.Second)
	for i := 0; i < attempts; i++ {
		select {
		case err := <-results:
			if !errors.Is(err, ErrTransitionBusy) {
				t.Fatalf("queued request %d error=%v, want ErrTransitionBusy", i, err)
			}
		case <-deadline:
			t.Fatalf("same-user requests formed a wait queue; only %d/%d returned", i, attempts)
		}
	}

	close(runtime.createGate)
	first := <-firstDone
	if first.err != nil || first.snapshot == nil {
		t.Fatalf("first transition snapshot=%+v err=%v", first.snapshot, first.err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := svc.WaitForProvisioning(waitCtx); err != nil {
		t.Fatalf("bounded transition admission did not drain: %v", err)
	}
}

func TestDurableStartReplayReturnsSameSnapshotWithoutSecondProviderCreate(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	first, err := svc.StartProblemOperation(context.Background(), "user-1", "p1", "start:p1:operation-replay")
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.StartProblemOperation(context.Background(), "user-1", "p1", "start:p1:operation-replay")
	if err != nil {
		t.Fatalf("replay start: %v", err)
	}
	if first.OperationID != second.OperationID || first.SessionID != second.SessionID || first.Generation != second.Generation ||
		!first.TimeoutAt.Equal(second.TimeoutAt) {
		t.Fatalf("replay changed snapshot: first=%+v second=%+v", first, second)
	}
	runtime.mu.Lock()
	created := len(runtime.created)
	runtime.mu.Unlock()
	if created != 1 {
		t.Fatalf("provider creates = %d, want 1", created)
	}
	durable.mu.Lock()
	reserveCalls := durable.reserveCalls
	durable.mu.Unlock()
	if reserveCalls != 1 {
		t.Fatalf("read-only projected replay repeated reservation mutation: calls=%d", reserveCalls)
	}
}

func TestDurableStartHoldsActiveCatalogLeaseThroughReservation(t *testing.T) {
	runtime := &mockRunner{running: true, setupGates: make(map[uint64]chan struct{})}
	problems := newMockProblemStore()
	problem := models.Problem{
		ID: "p1", Revision: "revision-p1", TimeoutMinutes: 30, VerifyType: "script",
	}
	problems.problems[problem.ID] = &problem
	catalog := newTestActiveProblemCatalog(problem)
	durable := &fakeDurableStore{}
	durable.reserveHook = func(params runner.ReserveSessionParams) error {
		if !catalog.isLeaseHeld() {
			return errors.New("active catalog lease was released before reservation commit")
		}
		if params.Selection != (runner.CatalogSelection{Generation: 1, Problem: runner.ProblemRef{ID: problem.ID, Revision: problem.Revision}}) {
			return fmt.Errorf("reservation selection = %+v", params.Selection)
		}
		return nil
	}
	svc, err := NewDurablePausedService(runtime, problems, catalog, durable, "local-docker:test")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Activate(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		svc.BeginShutdown()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = svc.WaitForWatchers(ctx)
	})

	if _, err := svc.StartProblemOperation(context.Background(), "user-1", "p1", "start:p1:catalog-lease"); err != nil {
		t.Fatalf("start with active catalog lease: %v", err)
	}
	if catalog.isLeaseHeld() {
		t.Fatal("active catalog lease remained held after durable reservation")
	}
}

func TestDurableStartReplayUsesHistoricalRevisionAfterRetirement(t *testing.T) {
	problem := models.Problem{
		ID: "p1", Revision: "revision-p1", TimeoutMinutes: 30, VerifyType: "script",
	}
	problems := newMockProblemStore()
	problems.problems[problem.ID] = &problem
	catalog := newTestActiveProblemCatalog(problem)
	durable := &fakeDurableStore{}
	operationID := "start:p1:retired-replay"

	firstRuntime := &mockRunner{running: true, setupGates: make(map[uint64]chan struct{})}
	first, err := NewDurablePausedService(firstRuntime, problems, catalog, durable, "local-docker:test")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Activate(); err != nil {
		t.Fatal(err)
	}
	firstSnapshot, err := first.StartProblemOperation(context.Background(), "user-1", "p1", operationID)
	if err != nil {
		t.Fatal(err)
	}
	first.setupWG.Wait()
	first.BeginShutdown()
	firstWait, firstCancel := context.WithTimeout(context.Background(), time.Second)
	defer firstCancel()
	if err := first.WaitForWatchers(firstWait); err != nil {
		t.Fatal(err)
	}

	catalog.mu.Lock()
	catalog.active = models.Problem{}
	catalog.activeErr = runner.ErrInvalidRevision
	acquiresBefore := catalog.acquireCalls
	resolvesBefore := catalog.resolveCalls
	catalog.mu.Unlock()

	secondRuntime := &mockRunner{running: true, setupGates: make(map[uint64]chan struct{})}
	second, err := NewDurablePausedService(secondRuntime, problems, catalog, durable, "local-docker:test")
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Activate(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		second.BeginShutdown()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = second.WaitForWatchers(ctx)
	})

	replayed, err := second.StartProblemOperation(context.Background(), "user-1", "p1", operationID)
	if err != nil {
		t.Fatalf("replay retired problem revision: %v", err)
	}
	if replayed.SessionID != firstSnapshot.SessionID || replayed.Generation != firstSnapshot.Generation ||
		!replayed.TimeoutAt.Equal(firstSnapshot.TimeoutAt) {
		t.Fatalf("retired replay changed identity: first=%+v replay=%+v", firstSnapshot, replayed)
	}
	catalog.mu.Lock()
	acquiresAfter := catalog.acquireCalls
	resolvesAfter := catalog.resolveCalls
	catalog.mu.Unlock()
	if acquiresAfter != acquiresBefore {
		t.Fatalf("exact replay consulted active catalog: before=%d after=%d", acquiresBefore, acquiresAfter)
	}
	if resolvesAfter != resolvesBefore+1 {
		t.Fatalf("exact replay historical resolutions = %d, want %d", resolvesAfter, resolvesBefore+1)
	}
}

func TestDurableStartReservationFailureNeverMutatesProvider(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	durable.reserveErr = errors.New("database unavailable")
	if _, err := svc.StartProblemOperation(context.Background(), "user-1", "p1", "start:p1:operation-db-failure"); err == nil {
		t.Fatal("expected durable reservation failure")
	}
	runtime.mu.Lock()
	created := len(runtime.created)
	runtime.mu.Unlock()
	if created != 0 {
		t.Fatalf("provider mutated before reservation commit: creates=%d", created)
	}
}

func TestDurableStartProviderFailurePersistsCleanupAndAbsence(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	runtime.createErr = errors.New("provider unavailable")
	if _, err := svc.StartProblemOperation(context.Background(), "user-1", "p1", "start:p1:operation-provider-failure"); err == nil {
		t.Fatal("expected provider failure")
	}
	durable.mu.Lock()
	createFailures, destroyed := durable.createFailures, durable.destroyedCalls
	order := append([]string(nil), durable.order...)
	durable.mu.Unlock()
	if createFailures != 1 || destroyed != 1 {
		t.Fatalf("failure convergence create_failed=%d destroyed=%d order=%v", createFailures, destroyed, order)
	}
	runtime.mu.Lock()
	removed := len(runtime.removed)
	runtime.mu.Unlock()
	if removed != 1 {
		t.Fatalf("exact provider cleanup calls = %d, want 1", removed)
	}
}

func TestDurableStartMismatchedProviderReferenceFailsCreateAndCleansReservedAllocation(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	wrong := runner.AllocationRef{
		ID: "99999999-9999-4999-8999-999999999999",
		Session: runner.SessionRef{
			SessionID:  "88888888-8888-4888-8888-888888888888",
			Generation: 99,
		},
		Provider: runner.ProviderLocalDocker,
	}
	runtime.createRef = &wrong

	_, err := svc.StartProblemOperation(context.Background(), "user-1", "p1", "start:p1:mismatched-provider-ref")
	if err == nil {
		t.Fatal("expected mismatched provider allocation failure")
	}

	durable.mu.Lock()
	reserved := durable.reservation.Allocation.Ref
	operationState := durable.reservation.Operation.State
	createFailures := durable.createFailures
	destroyRequests := durable.destroyRequest
	destroyedCalls := durable.destroyedCalls
	durable.mu.Unlock()
	if createFailures != 1 || destroyRequests != 0 || destroyedCalls != 1 || operationState != "failed" {
		t.Fatalf("mismatch convergence create_failed=%d destroy_requests=%d destroyed=%d operation=%s",
			createFailures, destroyRequests, destroyedCalls, operationState)
	}
	runtime.mu.Lock()
	removed := append([]runner.AllocationRef(nil), runtime.removed...)
	runtime.mu.Unlock()
	if len(removed) != 1 || removed[0] != reserved {
		t.Fatalf("mismatch cleanup targeted %+v, want exact reservation %+v", removed, reserved)
	}
	if removed[0] == wrong {
		t.Fatalf("mismatch cleanup trusted unowned provider response: %+v", wrong)
	}
}

func TestDurableResetDestroysOldBeforeCreatingOneReplacement(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	svc.SetStartCooldown(0)
	first, err := svc.StartProblemOperation(context.Background(), "user-1", "p1", "start:p1:before-reset")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, svc, "user-1", StatusReady)
	// StatusReady is stored immediately before the final ready callback. Wait
	// for generation-one setup to leave its goroutine before replacing callback
	// hooks so this test observes only generation-two publication order.
	svc.setupWG.Wait()
	var callbackMu sync.Mutex
	var callbacks []string
	svc.SetResetCallback(func(_ string, ref runner.SessionRef) {
		callbackMu.Lock()
		callbacks = append(callbacks, fmt.Sprintf("reset:%d", ref.Generation))
		callbackMu.Unlock()
	})
	svc.SetStageCallback(func(_ string, ref runner.SessionRef, stage, _ string) {
		callbackMu.Lock()
		callbacks = append(callbacks, fmt.Sprintf("stage:%d:%s", ref.Generation, stage))
		callbackMu.Unlock()
	})

	runtime.createEntered = make(chan runner.CreateSessionRequest, 1)
	runtime.createGate = make(chan struct{})
	type result struct {
		snapshot *CurrentSession
		err      error
	}
	done := make(chan result, 1)
	go func() {
		snapshot, resetErr := svc.ResetEnvironmentOperation(context.Background(), "user-1", "p1", runner.SessionRef{SessionID: first.SessionID, Generation: 1}, "reset:p1:operation-12345678")
		done <- result{snapshot: snapshot, err: resetErr}
	}()

	select {
	case request := <-runtime.createEntered:
		if request.Session.SessionID != first.SessionID || request.Session.Generation != 2 {
			t.Fatalf("replacement create request = %+v", request)
		}
		runtime.mu.Lock()
		removed := append([]runner.AllocationRef(nil), runtime.removed...)
		runtime.mu.Unlock()
		durable.mu.Lock()
		resetCalls, destroyedCalls := durable.resetCalls, durable.destroyedCalls
		durable.mu.Unlock()
		if len(removed) != 1 || removed[0].Session.Generation != 1 || resetCalls != 1 || destroyedCalls != 1 {
			t.Fatalf("replacement started before old absence: removed=%+v reset=%d destroyed=%d", removed, resetCalls, destroyedCalls)
		}
		callbackMu.Lock()
		beforeCreateReturns := append([]string(nil), callbacks...)
		callbackMu.Unlock()
		if len(beforeCreateReturns) != 1 || beforeCreateReturns[0] != "reset:2" {
			t.Fatalf("generation advance was not published before replacement progress: %v", beforeCreateReturns)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement provider create was not called")
	}
	close(runtime.createGate)
	resetResult := <-done
	if resetResult.err != nil {
		t.Fatalf("durable reset: %v", resetResult.err)
	}
	if resetResult.snapshot == nil || resetResult.snapshot.SessionID != first.SessionID || resetResult.snapshot.Generation != 2 ||
		resetResult.snapshot.OperationID == first.OperationID {
		t.Fatalf("replacement snapshot = %+v, original=%+v", resetResult.snapshot, first)
	}
	waitForStatus(t, svc, "user-1", StatusReady)
	callbackMu.Lock()
	orderedCallbacks := append([]string(nil), callbacks...)
	callbackMu.Unlock()
	if len(orderedCallbacks) < 2 || orderedCallbacks[0] != "reset:2" {
		t.Fatalf("reset callback must precede every generation-2 stage: %v", orderedCallbacks)
	}
	for _, callback := range orderedCallbacks[1:] {
		if callback == "reset:2" {
			t.Fatalf("generation advance was published more than once: %v", orderedCallbacks)
		}
		if !strings.HasPrefix(callback, "stage:2:") {
			t.Fatalf("unexpected callback after reset: %v", orderedCallbacks)
		}
	}

	replay, err := svc.ResetEnvironmentOperation(context.Background(), "user-1", "p1", runner.SessionRef{SessionID: first.SessionID, Generation: 1}, "reset:p1:operation-12345678")
	if err != nil {
		t.Fatalf("reset replay: %v", err)
	}
	if replay.OperationID != resetResult.snapshot.OperationID || replay.Generation != 2 {
		t.Fatalf("reset replay changed replacement: first=%+v replay=%+v", resetResult.snapshot, replay)
	}
	runtime.mu.Lock()
	creates, destroys := len(runtime.created), len(runtime.removed)
	runtime.mu.Unlock()
	if creates != 2 || destroys != 1 {
		t.Fatalf("reset replay repeated provider mutation: creates=%d destroys=%d", creates, destroys)
	}
}

func TestDurableResetReturnsPendingSnapshotWhenWorkerOwnsOldDestroy(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	svc.SetStartCooldown(0)
	started, err := svc.StartProblemOperation(context.Background(), "user-1", "p1", "start:p1:pending-reset-source")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, svc, "user-1", StatusReady)

	operationID := "reset:p1:pending-destroy-12345678"
	providerKey := "reset:" + runner.SessionIDForOperation("user-1", operationID)
	reset, err := durable.ReserveReset(context.Background(), runner.ReserveResetParams{
		Expected: runner.SessionRef{SessionID: started.SessionID, Generation: 1},
		UserID:   "user-1", Provider: runner.ProviderLocalDocker,
		ProviderID: "local-docker:test", IdempotencyKey: providerKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	backgroundClaim, found, err := durable.ClaimDestroyWork(
		context.Background(), "local-docker:test", reset.Old.ID, "background-worker", durableDestroyLease,
	)
	if err != nil || !found || backgroundClaim.Pending || backgroundClaim.Completed {
		t.Fatalf("background destroy claim found=%v claim=%+v err=%v", found, backgroundClaim, err)
	}

	pending, err := svc.ResetEnvironmentOperation(
		context.Background(), "user-1", "p1",
		runner.SessionRef{SessionID: started.SessionID, Generation: 1}, operationID,
	)
	if !errors.Is(err, ErrCleanupPending) {
		t.Fatalf("reset error=%v, want ErrCleanupPending", err)
	}
	if pending == nil || !pending.CleanupPending || pending.SessionID != started.SessionID ||
		pending.Generation != 2 || pending.OperationID != reset.New.Operation.ID || pending.Status != Status("queued") ||
		pending.EventSequence != 2 {
		t.Fatalf("pending reset snapshot=%+v", pending)
	}

	if err := runtime.DestroySession(context.Background(), backgroundClaim.Ref); err != nil {
		t.Fatal(err)
	}
	if err := durable.MarkClaimedDestroyed(context.Background(), backgroundClaim); err != nil {
		t.Fatal(err)
	}
	worked, err := svc.processDurableWorkOnce(context.Background())
	if err != nil || !worked {
		t.Fatalf("resume replacement worked=%v err=%v", worked, err)
	}
	waitForStatus(t, svc, "user-1", StatusReady)
	runtime.mu.Lock()
	creates, destroys := len(runtime.created), len(runtime.removed)
	runtime.mu.Unlock()
	if creates != 2 || destroys != 1 {
		t.Fatalf("pending reset effects create=%d destroy=%d, want 2/1", creates, destroys)
	}
}

func TestDurableShutdownPersistsEveryCleanupIntentBeforeProviderDrain(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	svc.SetStartCooldown(0)
	const sessions = 9
	for index := 0; index < sessions; index++ {
		userID := fmt.Sprintf("shutdown-user-%02d", index)
		operationID := fmt.Sprintf("start:p1:shutdown-%08d", index)
		if _, err := svc.StartProblemOperation(context.Background(), userID, "p1", operationID); err != nil {
			t.Fatalf("start %s: %v", userID, err)
		}
		waitForStatus(t, svc, userID, StatusReady)
	}

	runtime.destroyEntered = make(chan runner.AllocationRef, sessions)
	runtime.destroyGate = make(chan struct{})
	svc.BeginShutdown()
	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- svc.CleanupAll(context.Background()) }()

	select {
	case <-runtime.destroyEntered:
	case <-time.After(time.Second):
		t.Fatal("shutdown provider drain did not start")
	}
	durable.mu.Lock()
	destroyRequests := durable.destroyRequest
	pending := len(durable.pendingDestroy)
	durable.mu.Unlock()
	if destroyRequests != sessions || pending != sessions {
		t.Fatalf("provider drain began before full intent phase: requests=%d pending=%d want=%d", destroyRequests, pending, sessions)
	}

	close(runtime.destroyGate)
	select {
	case err := <-cleanupDone:
		if err != nil {
			t.Fatalf("durable shutdown cleanup: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("durable shutdown cleanup did not drain")
	}
	runtime.mu.Lock()
	removed := len(runtime.removed)
	runtime.mu.Unlock()
	if removed != sessions {
		t.Fatalf("shutdown provider removals=%d, want=%d", removed, sessions)
	}
	durable.mu.Lock()
	pending = len(durable.pendingDestroy)
	durable.mu.Unlock()
	if pending != 0 {
		t.Fatalf("shutdown retained %d pending destroys after successful drain", pending)
	}
	for index := 0; index < sessions; index++ {
		if current := svc.GetCurrentSession(fmt.Sprintf("shutdown-user-%02d", index)); current != nil {
			t.Fatalf("shutdown retained session %d: %+v", index, current)
		}
	}
}

func TestDurableResetDestroyFailurePreventsReplacementUntilSameKeyRetry(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	svc.SetStartCooldown(0)
	if _, err := svc.StartProblemOperation(context.Background(), "user-1", "p1", "start:p1:reset-failure"); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, svc, "user-1", StatusReady)
	runtime.destroyErr = errors.New("provider busy")
	key := "reset:p1:retry-destroy-12345678"
	current := svc.GetCurrentSession("user-1")
	if _, err := svc.ResetEnvironmentOperation(context.Background(), "user-1", "p1", runner.SessionRef{SessionID: current.SessionID, Generation: 1}, key); err == nil {
		t.Fatal("expected old allocation destroy failure")
	}
	runtime.mu.Lock()
	createsAfterFailure := len(runtime.created)
	runtime.mu.Unlock()
	if createsAfterFailure != 1 {
		t.Fatalf("replacement created before old absence: creates=%d", createsAfterFailure)
	}
	durable.mu.Lock()
	resetCallsAfterFailure := durable.resetCalls
	durable.mu.Unlock()
	if _, err := svc.ResetEnvironmentOperation(
		context.Background(),
		"user-1", "p1",
		runner.SessionRef{SessionID: current.SessionID, Generation: 2},
		"reset:p1:different-key-12345678",
	); !errors.Is(err, runner.ErrLifecycleConflict) {
		t.Fatalf("different-key reset during cleanup error = %v, want lifecycle conflict", err)
	}
	durable.mu.Lock()
	resetCallsAfterDifferentKey := durable.resetCalls
	durable.mu.Unlock()
	runtime.mu.Lock()
	createsAfterDifferentKey, destroysAfterDifferentKey := len(runtime.created), len(runtime.removed)
	runtime.mu.Unlock()
	if resetCallsAfterDifferentKey != resetCallsAfterFailure || createsAfterDifferentKey != 1 || destroysAfterDifferentKey != 1 {
		t.Fatalf("different key bypassed pending cleanup: reset=%d/%d creates=%d destroys=%d",
			resetCallsAfterDifferentKey, resetCallsAfterFailure, createsAfterDifferentKey, destroysAfterDifferentKey)
	}

	retry, err := svc.ResetEnvironmentOperation(context.Background(), "user-1", "p1", runner.SessionRef{SessionID: current.SessionID, Generation: 1}, key)
	if err != nil {
		t.Fatalf("same-key reset retry: %v", err)
	}
	if retry.Generation != 2 {
		t.Fatalf("same-key retry advanced again: %+v", retry)
	}
	runtime.mu.Lock()
	creates, destroys := len(runtime.created), len(runtime.removed)
	runtime.mu.Unlock()
	if creates != 2 || destroys != 2 {
		t.Fatalf("retry mutation counts creates=%d destroys=%d", creates, destroys)
	}
}

func TestDurableWorkerConvergesResetAfterDestroyFailureWithoutHTTPRetry(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	svc.SetStartCooldown(0)
	started, err := svc.StartProblemOperation(context.Background(), "user-1", "p1", "start:p1:worker-recovery")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, svc, "user-1", StatusReady)

	runtime.destroyErr = errors.New("provider temporarily busy")
	_, err = svc.ResetEnvironmentOperation(
		context.Background(), "user-1", "p1",
		runner.SessionRef{SessionID: started.SessionID, Generation: 1},
		"reset:p1:worker-recovery-12345678",
	)
	if err == nil {
		t.Fatal("expected synchronous reset destroy failure")
	}
	current := svc.GetCurrentSession("user-1")
	if current == nil || current.Generation != 2 || current.Status != StatusCreating {
		t.Fatalf("failed reset did not retain the durable replacement: %+v", current)
	}
	runtime.mu.Lock()
	createsBeforeWorker, destroysBeforeWorker := len(runtime.created), len(runtime.removed)
	runtime.mu.Unlock()
	if createsBeforeWorker != 1 || destroysBeforeWorker != 1 {
		t.Fatalf("provider calls before worker create=%d destroy=%d", createsBeforeWorker, destroysBeforeWorker)
	}

	worked, err := svc.processDurableWorkOnce(context.Background())
	if err != nil {
		t.Fatalf("process durable recovery work: %v", err)
	}
	if !worked {
		t.Fatal("durable worker found no reset recovery work")
	}
	waitForStatus(t, svc, "user-1", StatusReady)

	runtime.mu.Lock()
	creates, destroys := len(runtime.created), len(runtime.removed)
	created := append([]runner.CreateSessionRequest(nil), runtime.created...)
	runtime.mu.Unlock()
	if creates != 2 || destroys != 2 {
		t.Fatalf("worker recovery provider calls create=%d destroy=%d, want 2/2", creates, destroys)
	}
	if created[1].Session.Generation != 2 || created[1].AllocationID != runner.AllocationIDForSession(created[1].Session) {
		t.Fatalf("worker created wrong replacement: %+v", created[1])
	}
	durable.mu.Lock()
	pendingDestroy := len(durable.pendingDestroy)
	destroyedCalls := durable.destroyedCalls
	durable.mu.Unlock()
	if pendingDestroy != 0 || destroyedCalls != 1 {
		t.Fatalf("worker durable cleanup pending=%d destroyed=%d", pendingDestroy, destroyedCalls)
	}

	worked, err = svc.processDurableWorkOnce(context.Background())
	if err != nil || worked {
		t.Fatalf("idle worker result worked=%v err=%v", worked, err)
	}
	runtime.mu.Lock()
	createsAfterReplay, destroysAfterReplay := len(runtime.created), len(runtime.removed)
	runtime.mu.Unlock()
	if createsAfterReplay != creates || destroysAfterReplay != destroys {
		t.Fatalf("idle worker repeated provider mutation create=%d/%d destroy=%d/%d",
			createsAfterReplay, creates, destroysAfterReplay, destroys)
	}
}

func TestDurableResetHistoricalReplayCannotRegressCurrentGeneration(t *testing.T) {
	svc, runtime, _, _ := newDurableTestService(t)
	svc.SetStartCooldown(0)
	started, err := svc.StartProblemOperation(context.Background(), "user-1", "p1", "start:p1:historical-reset")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, svc, "user-1", StatusReady)

	firstKey := "reset:p1:first-historical-12345678"
	firstReset, err := svc.ResetEnvironmentOperation(
		context.Background(), "user-1", "p1",
		runner.SessionRef{SessionID: started.SessionID, Generation: 1}, firstKey,
	)
	if err != nil {
		t.Fatalf("first reset: %v", err)
	}
	waitForStatus(t, svc, "user-1", StatusReady)
	secondReset, err := svc.ResetEnvironmentOperation(
		context.Background(), "user-1", "p1",
		runner.SessionRef{SessionID: started.SessionID, Generation: 2},
		"reset:p1:second-historical-12345678",
	)
	if err != nil {
		t.Fatalf("second reset: %v", err)
	}
	waitForStatus(t, svc, "user-1", StatusReady)

	runtime.mu.Lock()
	createsBefore, destroysBefore := len(runtime.created), len(runtime.removed)
	runtime.mu.Unlock()
	historical, err := svc.ResetEnvironmentOperation(
		context.Background(), "user-1", "p1",
		runner.SessionRef{SessionID: started.SessionID, Generation: 1}, firstKey,
	)
	if err != nil {
		t.Fatalf("historical reset replay: %v", err)
	}
	if historical.OperationID != firstReset.OperationID || historical.Generation != 2 {
		t.Fatalf("historical operation snapshot = %+v, want operation %s generation 2", historical, firstReset.OperationID)
	}
	current := svc.GetCurrentSession("user-1")
	if current == nil || current.Generation != 3 || current.OperationID != secondReset.OperationID || current.Status != StatusReady {
		t.Fatalf("historical replay regressed current session: %+v", current)
	}
	runtime.mu.Lock()
	createsAfter, destroysAfter := len(runtime.created), len(runtime.removed)
	runtime.mu.Unlock()
	if createsAfter != createsBefore || destroysAfter != destroysBefore {
		t.Fatalf("historical replay mutated provider: creates=%d/%d destroys=%d/%d",
			createsAfter, createsBefore, destroysAfter, destroysBefore)
	}
}

func TestProvisionableCreateSkipsContendedUserAndServesNextUser(t *testing.T) {
	svc, runtime, problems, durable := newDurableTestService(t)
	now := time.Now()
	reserve := func(sessionID, userID, key string, queuedAt time.Time) runner.SessionReservation {
		reservation, err := durable.ReserveSession(context.Background(), runner.ReserveSessionParams{
			SessionID: sessionID, UserID: userID,
			Selection: runner.CatalogSelection{Generation: 1, Problem: runner.ProblemRef{ID: "p1", Revision: "revision-p1"}},
			Provider:  runner.ProviderLocalDocker, ProviderID: "local-docker:test",
			ResourceProfile: runner.DefaultResourceProfile, ExpiresAt: now.Add(time.Hour),
			IdempotencyKey: key,
		})
		if err != nil {
			t.Fatalf("reserve %s: %v", userID, err)
		}
		durable.mu.Lock()
		durable.updateReservation(reservation.Allocation.Ref, func(stored *runner.SessionReservation) {
			stored.Session.QueuedAt = queuedAt
		})
		reservation.Session.QueuedAt = queuedAt
		durable.mu.Unlock()
		return reservation
	}
	a := reserve("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "user-a", "start:p1:fair-a", now.Add(-time.Minute))
	b := reserve("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "user-b", "start:p1:fair-b", now)

	problem := problems.problems["p1"]
	for _, reservation := range []runner.SessionReservation{a, b} {
		svc.sessions[reservation.Session.UserID] = &Session{
			ID: reservation.Session.ID, UserID: reservation.Session.UserID,
			ProblemID: problem.ID, Selection: reservation.Session.Selection,
			VerifyType: problem.VerifyType, AttemptID: reservation.AttemptID,
			OperationID: reservation.Operation.ID, Generation: reservation.Session.CurrentGeneration,
			Allocation: reservation.Allocation.Ref, Status: StatusCreating,
			StartedAt: reservation.Session.QueuedAt, TimeoutAt: reservation.Session.ExpiresAt,
		}
	}

	aLock := svc.userLock("user-a")
	aLock.Lock()
	defer aLock.Unlock()
	worked, err := svc.processProvisionableCreateOnce(context.Background())
	if err != nil || !worked {
		t.Fatalf("process create behind contended user worked=%v err=%v", worked, err)
	}
	waitForStatus(t, svc, "user-b", StatusReady)
	runtime.mu.Lock()
	created := append([]runner.CreateSessionRequest(nil), runtime.created...)
	runtime.mu.Unlock()
	if len(created) != 1 || created[0].UserID != "user-b" || created[0].AllocationID != b.Allocation.Ref.ID {
		t.Fatalf("create worker did not skip contended user-a: %+v", created)
	}
	if current := svc.GetCurrentSession("user-a"); current == nil || current.Status != StatusCreating {
		t.Fatalf("contended user-a was mutated: %+v", current)
	}
}
