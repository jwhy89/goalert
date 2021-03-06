package escalationmanager

import (
	"context"
	"database/sql"
	alertlog "github.com/target/goalert/alert/log"
	"github.com/target/goalert/permission"
	"github.com/target/goalert/util/log"

	"github.com/pkg/errors"
)

// UpdateAll will update the state of all active escalation policies.
func (db *DB) UpdateAll(ctx context.Context) error {
	err := db.update(ctx, true, nil)
	return err
}

func (db *DB) update(ctx context.Context, all bool, alertID *int) error {
	err := permission.LimitCheckAny(ctx, permission.System)
	if err != nil {
		return err
	}
	log.Debugf(ctx, "Updating alert escalations.")

	tx, err := db.lock.BeginTx(ctx, nil)
	if err != nil {
		return errors.Wrap(err, "begin tx")
	}
	defer tx.Rollback()

	_, err = tx.StmtContext(ctx, db.lockStmt).ExecContext(ctx)
	if err != nil {
		return errors.Wrap(err, "lock ep step table")
	}
	_, err = tx.StmtContext(ctx, db.updateOnCall).ExecContext(ctx)
	if err != nil {
		return errors.Wrap(err, "update ep step on-call")
	}
	err = tx.Commit()
	if err != nil {
		return errors.Wrap(err, "commit on-call update")
	}

	_, err = db.lock.Exec(ctx, db.cleanupNoSteps)
	if err != nil {
		return errors.Wrap(err, "end policies with no steps")
	}

	err = db.processEscalations(ctx, db.newPolicies, func(rows *sql.Rows) (int, *alertlog.EscalationMetaData, error) {
		var id int
		err := rows.Scan(&id)
		return id, &alertlog.EscalationMetaData{}, err
	})
	if err != nil {
		return errors.Wrap(err, "trigger new policies")
	}

	err = db.processEscalations(ctx, db.deletedSteps, func(rows *sql.Rows) (int, *alertlog.EscalationMetaData, error) {
		var id int
		var meta alertlog.EscalationMetaData
		err := rows.Scan(&id, &meta.Repeat, &meta.NewStepIndex)
		return id, &meta, err
	})
	if err != nil {
		return errors.Wrap(err, "escalate policies with deleted steps")
	}

	err = db.processEscalations(ctx, db.normalEscalation, func(rows *sql.Rows) (int, *alertlog.EscalationMetaData, error) {
		var id int
		var meta alertlog.EscalationMetaData
		err := rows.Scan(&id, &meta.Repeat, &meta.NewStepIndex, &meta.OldDelayMinutes, &meta.Forced)
		return id, &meta, err
	})
	if err != nil {
		return errors.Wrap(err, "escalate forced or expired")
	}

	return nil
}

func (db *DB) processEscalations(ctx context.Context, stmt *sql.Stmt, scan func(*sql.Rows) (int, *alertlog.EscalationMetaData, error)) error {
	tx, err := db.lock.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.StmtContext(ctx, stmt).QueryContext(ctx)
	if err != nil {
		return err
	}
	defer rows.Close()

	type record struct {
		alertID int
		esc     *alertlog.EscalationMetaData
	}

	var data []record
	for rows.Next() {
		var rec record
		rec.alertID, rec.esc, err = scan(rows)
		if err != nil {
			return err
		}
		data = append(data, rec)
	}

	for _, rec := range data {
		err = db.log.LogTx(ctx, tx, rec.alertID, alertlog.TypeEscalated, rec.esc)
		if err != nil {
			return errors.Wrap(err, "log escalation")
		}
	}

	return tx.Commit()
}
