package runner

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrControllerLeaseHeld = errors.New("runner controller lease is already held")
	ErrControllerLeaseLost = errors.New("runner controller lease was lost")
)

const defaultLeaseHealthInterval = 2 * time.Second

// ControllerLease holds a PostgreSQL session advisory lock on one dedicated
// connection. It fences startup/reconciliation for a provider instance; it is
// not a substitute for generation CAS or provider idempotency.
type ControllerLease struct {
	conn         *pgxpool.Conn
	providerID   string
	epoch        uint64
	leaseID      string
	key          int64
	backendPID   int32
	backendStart time.Time
	onLost       func(error)
	closeReq     chan struct{}
	lossReq      chan error
	done         chan struct{}
	lost         chan error

	// sessionMu serializes every use and final disposition of the dedicated
	// pgx connection. pgx.Conn is not safe for concurrent use.
	sessionMu sync.Mutex
	mu        sync.Mutex
	state     controllerLeaseState
	result    error
}

type controllerLeaseState uint8

const (
	controllerLeaseActive controllerLeaseState = iota
	controllerLeaseClosing
	controllerLeaseLost
	controllerLeaseDone
)

// AcquireControllerLease fails immediately when another process owns the same
// provider instance. The dedicated connection is retained until Close; losing
// it releases the PostgreSQL advisory lock and signals Lost.
func AcquireControllerLease(ctx context.Context, pool *pgxpool.Pool, providerID string, onLost func(error)) (*ControllerLease, error) {
	return acquireControllerLease(ctx, pool, providerID, defaultLeaseHealthInterval, onLost)
}

func acquireControllerLease(ctx context.Context, pool *pgxpool.Pool, providerID string, healthInterval time.Duration, onLost func(error)) (*ControllerLease, error) {
	providerID = strings.TrimSpace(providerID)
	if pool == nil {
		return nil, errors.New("runner controller lease requires a database pool")
	}
	if providerID == "" || len(providerID) > 128 {
		return nil, errors.New("runner controller lease provider id is invalid")
	}
	if healthInterval <= 0 {
		return nil, errors.New("runner controller lease health interval must be positive")
	}
	if onLost == nil {
		return nil, errors.New("runner controller lease requires a synchronous loss fence")
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire runner controller lease connection: %w", err)
	}
	key := controllerLeaseKey(providerID)
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&acquired); err != nil {
		_ = discardLeaseConnection(conn, defaultLeaseHealthInterval)
		return nil, fmt.Errorf("acquire runner controller advisory lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return nil, fmt.Errorf("%w for provider %q", ErrControllerLeaseHeld, providerID)
	}
	leaseID, err := newUUID()
	if err != nil {
		_ = discardLeaseConnection(conn, defaultLeaseHealthInterval)
		return nil, fmt.Errorf("generate runner controller lease id: %w", err)
	}
	var epoch int64
	var backendPID int32
	var backendStart time.Time
	if err := conn.QueryRow(ctx, `
		WITH identity AS MATERIALIZED (
			SELECT pg_backend_pid() AS pid, activity.backend_start
			FROM pg_catalog.pg_stat_activity activity
			WHERE activity.pid=pg_backend_pid()
		), held AS MATERIALIZED (
			SELECT identity.pid,identity.backend_start
			FROM identity
			WHERE EXISTS (
				SELECT 1
				FROM pg_catalog.pg_locks held_lock
				WHERE held_lock.pid=identity.pid
				  AND held_lock.locktype='advisory'
				  AND held_lock.database=(
					  SELECT oid FROM pg_catalog.pg_database
					  WHERE datname=current_database()
				  )
				  AND held_lock.classid::BIGINT=(($2::BIGINT >> 32) & 4294967295)
				  AND held_lock.objid::BIGINT=($2::BIGINT & 4294967295)
				  AND held_lock.objsubid=1
				  AND held_lock.mode='ExclusiveLock'
				  AND held_lock.granted
			)
		)
		INSERT INTO runner_controller_epochs AS current (
			provider_id,epoch,lease_id,lease_backend_pid,lease_backend_start,
			lease_advisory_key,updated_at
		)
		SELECT $1,1,$3::UUID,held.pid,held.backend_start,$2,clock_timestamp()
		FROM held
		ON CONFLICT (provider_id) DO UPDATE SET
			epoch=current.epoch+1,
			lease_id=EXCLUDED.lease_id,
			lease_backend_pid=EXCLUDED.lease_backend_pid,
			lease_backend_start=EXCLUDED.lease_backend_start,
			lease_advisory_key=EXCLUDED.lease_advisory_key,
			updated_at=clock_timestamp()
		WHERE current.epoch < 9223372036854775807
		RETURNING epoch,lease_backend_pid,lease_backend_start`, providerID, key, leaseID).Scan(
		&epoch, &backendPID, &backendStart,
	); err != nil {
		_ = discardLeaseConnection(conn, defaultLeaseHealthInterval)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errors.New("runner controller authority could not be issued")
		}
		return nil, fmt.Errorf("issue runner controller epoch: %w", err)
	}
	if epoch <= 0 {
		_ = discardLeaseConnection(conn, defaultLeaseHealthInterval)
		return nil, errors.New("issued runner controller epoch is invalid")
	}

	lease := &ControllerLease{
		conn: conn, providerID: providerID, epoch: uint64(epoch), leaseID: leaseID,
		key: key, backendPID: backendPID, backendStart: backendStart.UTC(), onLost: onLost,
		closeReq: make(chan struct{}), done: make(chan struct{}), lost: make(chan error, 1),
		lossReq: make(chan error, 1),
		state:   controllerLeaseActive,
	}
	go lease.monitor(healthInterval)
	return lease, nil
}

func (l *ControllerLease) Fence() ControllerFence {
	if l == nil {
		return ControllerFence{}
	}
	return ControllerFence{ProviderID: l.providerID, Epoch: l.epoch}
}

// Lost reports an unexpected loss of the dedicated PostgreSQL session. Callers
// must stop admission and provider mutation when it yields an error.
func (l *ControllerLease) Lost() <-chan error {
	return l.lost
}

// Close releases the advisory lock and dedicated pool connection. It is safe
// to call more than once; subsequent calls wait for the same shutdown.
func (l *ControllerLease) Close(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if l.state == controllerLeaseActive {
		l.state = controllerLeaseClosing
		close(l.closeReq)
	}
	l.mu.Unlock()
	select {
	case <-l.done:
		l.mu.Lock()
		result := l.result
		l.mu.Unlock()
		return result
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *ControllerLease) monitor(interval time.Duration) {
	defer close(l.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-l.closeReq:
			l.finishNormalClose(interval)
			return
		case loss := <-l.lossReq:
			l.finishUnexpectedLoss(loss, interval)
			return
		case <-ticker.C:
			l.sessionMu.Lock()
			l.mu.Lock()
			active := l.state == controllerLeaseActive
			l.mu.Unlock()
			if !active {
				l.sessionMu.Unlock()
				continue
			}
			probeCtx, cancel := context.WithTimeout(context.Background(), interval)
			alive, err := l.probeSession(probeCtx)
			cancel()
			if err != nil || alive != 1 {
				cause := err
				if cause == nil {
					cause = errors.New("controller lease health probe returned an invalid value")
				}
				loss := fmt.Errorf("%w: %v", ErrControllerLeaseLost, cause)
				requested := l.requestLossLocked(loss)
				l.sessionMu.Unlock()
				if requested {
					l.finishUnexpectedLoss(loss, interval)
					return
				}
				continue
			}
			l.sessionMu.Unlock()
		}
	}
}

// requestLossLocked transitions active authority to lost while sessionMu is
// held, preventing another proof issuance from entering between a failed exact
// probe and the state transition.
func (l *ControllerLease) requestLossLocked(loss error) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != controllerLeaseActive {
		return false
	}
	l.state = controllerLeaseLost
	return true
}

func (l *ControllerLease) finishUnexpectedLoss(loss error, interval time.Duration) {
	callbackErr := invokeLeaseLossFence(l.onLost, loss)
	l.sessionMu.Lock()
	discardErr := discardLeaseConnection(l.conn, interval)
	l.sessionMu.Unlock()
	result := errors.Join(loss, callbackErr, discardErr)
	l.mu.Lock()
	l.result = result
	l.state = controllerLeaseDone
	l.mu.Unlock()
	l.lost <- result
}

// signalLossLocked schedules unexpected-loss finalization. sessionMu must be
// held and requestLossLocked must have returned true.
func (l *ControllerLease) signalLossLocked(loss error) {
	select {
	case l.lossReq <- loss:
	default:
	}
}

func (l *ControllerLease) finishNormalClose(interval time.Duration) {
	l.sessionMu.Lock()
	defer l.sessionMu.Unlock()

	unlockCtx, cancel := context.WithTimeout(context.Background(), interval)
	tx, err := l.conn.BeginTx(unlockCtx, pgx.TxOptions{})
	if err == nil {
		var cleared int
		err = tx.QueryRow(unlockCtx, `
			UPDATE runner_controller_epochs
			SET lease_id=NULL,
			    lease_backend_pid=NULL,
			    lease_backend_start=NULL,
			    lease_advisory_key=NULL,
			    updated_at=clock_timestamp()
			WHERE provider_id=$1
			  AND epoch=$2
			  AND lease_id=$3::UUID
			  AND lease_backend_pid=$4
			  AND lease_backend_start=$5
			  AND lease_advisory_key=$6
			RETURNING 1`,
			l.providerID, int64(l.epoch), l.leaseID, l.backendPID, l.backendStart, l.key,
		).Scan(&cleared)
		if err == nil && cleared != 1 {
			err = errors.New("runner controller lease metadata was not cleared")
		}
		var unlocked bool
		if err == nil {
			err = tx.QueryRow(unlockCtx, `SELECT pg_advisory_unlock($1)`, l.key).Scan(&unlocked)
			if err == nil && !unlocked {
				err = errors.New("runner controller advisory lock was not held during close")
			}
		}
		if err == nil {
			err = tx.Commit(unlockCtx)
		} else {
			_ = tx.Rollback(unlockCtx)
		}
	}
	cancel()
	if err == nil {
		l.conn.Release()
	} else {
		err = errors.Join(err, discardLeaseConnection(l.conn, interval))
	}
	l.mu.Lock()
	l.result = err
	l.state = controllerLeaseDone
	l.mu.Unlock()
}

// probeSession checks the exact epoch row and the exact session-level advisory
// lock. Calling pg_try_advisory_lock here would be unsafe because advisory
// locks are reentrant and would increase the required unlock count.
func (l *ControllerLease) probeSession(ctx context.Context) (int, error) {
	var alive int
	err := l.conn.QueryRow(ctx, `
		SELECT CASE WHEN EXISTS (
			SELECT 1
			FROM runner_controller_epochs authority
			WHERE authority.provider_id=$1
			  AND authority.epoch=$2
			  AND authority.lease_id=$3::UUID
			  AND authority.lease_backend_pid=pg_backend_pid()
			  AND authority.lease_backend_start=$4
			  AND authority.lease_advisory_key=$5
			  AND EXISTS (
				SELECT 1
				FROM pg_catalog.pg_locks held_lock
				WHERE held_lock.pid=pg_backend_pid()
				  AND held_lock.locktype='advisory'
				  AND held_lock.database=(
					  SELECT oid FROM pg_catalog.pg_database
					  WHERE datname=current_database()
				  )
				  AND held_lock.classid::BIGINT=(($5::BIGINT >> 32) & 4294967295)
				  AND held_lock.objid::BIGINT=($5::BIGINT & 4294967295)
				  AND held_lock.objsubid=1
				  AND held_lock.mode='ExclusiveLock'
				  AND held_lock.granted
			  )
		) THEN 1 ELSE 0 END`,
		l.providerID, int64(l.epoch), l.leaseID, l.backendStart, l.key,
	).Scan(&alive)
	return alive, err
}

func discardLeaseConnection(conn *pgxpool.Conn, timeout time.Duration) error {
	if conn == nil {
		return nil
	}
	raw := conn.Hijack()
	closeCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return raw.Close(closeCtx)
}

func invokeLeaseLossFence(callback func(error), loss error) (callbackErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			callbackErr = fmt.Errorf("runner controller loss fence panicked: %v", recovered)
		}
	}()
	callback(loss)
	return nil
}

func controllerLeaseKey(providerID string) int64 {
	sum := sha256.Sum256([]byte("k8s-quiz/controller-lease/v1\x00" + providerID))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}
