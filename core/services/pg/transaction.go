package pg

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/pkg/errors"
	"github.com/smartcontractkit/chainlink/core/logger"
	"github.com/smartcontractkit/sqlx"
	"go.uber.org/multierr"
)

type TxOptions struct {
	sql.TxOptions
	LockTimeout            time.Duration
	IdleInTxSessionTimeout time.Duration
}

// NOTE: In an ideal world the timeouts below would be set to something sane in
// the postgres configuration by the user. Since we do not live in an ideal
// world, it is necessary to override them here.
//
// They cannot easily be set at a session level due to how Go's connection
// pooling works.
const (
	// DefaultLockTimeout controls the max time we will wait for any kind of database lock.
	// It's good to set this to _something_ because waiting for locks forever is really bad.
	DefaultLockTimeout = 15 * time.Second
	// DefaultIdleInTxSessionTimeout controls the max time we leave a transaction open and idle.
	// It's good to set this to _something_ because leaving transactions open forever is really bad.
	DefaultIdleInTxSessionTimeout = 1 * time.Hour
	// NOTE: This is the default level in Postgres anyway, we just make it
	// explicit here
	DefaultIsolation = sql.LevelReadCommitted
)

func OptReadOnlyTx() TxOptions {
	return TxOptions{TxOptions: sql.TxOptions{ReadOnly: true}}
}

func applyDefaults(optss []TxOptions) (lockTimeout, idleInTxSessionTimeout time.Duration, txOpts sql.TxOptions) {
	lockTimeout = DefaultLockTimeout
	idleInTxSessionTimeout = DefaultIdleInTxSessionTimeout
	txIsolation := DefaultIsolation
	readOnly := false
	if len(optss) > 0 {
		opts := optss[0]
		if opts.LockTimeout != 0 {
			lockTimeout = opts.LockTimeout
		}
		if opts.IdleInTxSessionTimeout != 0 {
			idleInTxSessionTimeout = opts.IdleInTxSessionTimeout
		}
		if opts.Isolation != 0 {
			txIsolation = opts.Isolation
		}
		readOnly = opts.ReadOnly
	}
	txOpts = sql.TxOptions{
		Isolation: txIsolation,
		ReadOnly:  readOnly,
	}
	return
}

func SqlTransaction(ctx context.Context, rdb *sql.DB, lggr logger.Logger, fn func(tx *sqlx.Tx) error, optss ...TxOptions) (err error) {
	db := WrapDbWithSqlx(rdb)
	return sqlxTransaction(ctx, db, lggr, fn, optss...)
}

func sqlxTransaction(ctx context.Context, db *sqlx.DB, lggr logger.Logger, fn func(tx *sqlx.Tx) error, optss ...TxOptions) (err error) {
	wrapFn := func(q Queryer) error {
		tx, ok := q.(*sqlx.Tx)
		if !ok {
			panic(fmt.Sprintf("expected q to be %T but got %T", tx, q))
		}
		return fn(tx)
	}
	return sqlxTransactionQ(ctx, db, lggr, wrapFn, optss...)
}

func sqlxTransactionQ(ctx context.Context, db *sqlx.DB, lggr logger.Logger, fn func(q Queryer) error, optss ...TxOptions) (err error) {
	lockTimeout, idleInTxSessionTimeout, txOpts := applyDefaults(optss)

	tx, err := db.BeginTxx(ctx, &txOpts)
	if err != nil {
		return errors.Wrap(err, "failed to begin transaction")
	}

	defer func() {
		if p := recover(); p != nil {
			sentry.CurrentHub().Recover(p)
			sentry.Flush(logger.SentryFlushDeadline)

			// A panic occurred, rollback and repanic
			lggr.Errorf("Panic in transaction, rolling back: %s", p)
			done := make(chan struct{})
			go func() {
				if rerr := tx.Rollback(); rerr != nil {
					lggr.Errorf("Failed to rollback on panic: %s", rerr)
				}
				close(done)
			}()
			select {
			case <-done:
				panic(p)
			case <-time.After(10 * time.Second):
				panic(fmt.Sprintf("panic in transaction; aborting rollback that took longer than 10s: %s", p))
			}
		} else if err != nil {
			lggr.Debugf("Error in transaction, rolling back: %s", err)
			// An error occurred, rollback and return error
			if rerr := tx.Rollback(); rerr != nil {
				err = multierr.Combine(err, errors.WithStack(rerr))
			}
		} else {
			// All good! Time to commit.
			err = errors.WithStack(tx.Commit())
		}
	}()

	if lockTimeout != DefaultLockTimeout {
		_, err = tx.Exec(fmt.Sprintf(`SET LOCAL lock_timeout = %d`, lockTimeout.Milliseconds()))
		if err != nil {
			return errors.Wrap(err, "error setting transaction local lock_timeout")
		}
	}
	if idleInTxSessionTimeout != DefaultIdleInTxSessionTimeout {
		_, err = tx.Exec(fmt.Sprintf(`SET LOCAL idle_in_transaction_session_timeout = %d`, idleInTxSessionTimeout.Milliseconds()))
		if err != nil {
			return errors.Wrap(err, "error setting transaction local idle_in_transaction_session_timeout")
		}
	}

	err = fn(tx)

	return
}
