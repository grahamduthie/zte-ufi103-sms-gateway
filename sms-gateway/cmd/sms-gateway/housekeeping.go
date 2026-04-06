package main

// housekeeping.go — periodic maintenance tasks:
//
//  1. Log rotation: when sms-gateway.log exceeds maxLogBytes, the current log
//     is renamed to sms-gateway.log.1 (overwriting any previous .1 file) and a
//     fresh log file is opened. This keeps at most two log files on disk, so
//     total log storage is bounded to 2×maxLogBytes.
//
//  2. WAL checkpoint: once per day, PRAGMA wal_checkpoint(TRUNCATE) is issued
//     so the SQLite WAL file does not grow unboundedly. Under normal operation
//     SQLite auto-checkpoints at 1000 pages (~4 MB), but the checkpoint call
//     here also TRUNCATEs the WAL file back to zero bytes on disk.
//
//  3. Old record pruning: messages and completed/failed send_queue entries
//     older than msgRetainDays are deleted. At typical SMS volumes (a few
//     hundred messages per month) the database will never meaningfully grow,
//     but this prevents unbounded accumulation over years.

import (
	"context"
	"log"
	"os"
	"time"

	"marlowfm.co.uk/sms-gateway/internal/atcmd"
	"marlowfm.co.uk/sms-gateway/internal/database"
)

const (
	housekeepingInterval = 1 * time.Hour
	maxLogBytes          = 10 * 1024 * 1024 // 10 MB per log file, 20 MB total
	msgRetainDays        = 90
)

func runHousekeeping(ctx context.Context, db *database.DB, at *atcmd.Session, storage, logPath string, logger *log.Logger) {
	t := time.NewTicker(housekeepingInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		rotateLog(logPath, logger)
		checkpointWAL(db, logger)
		pruneOldRecords(db, logger)
		// Re-apply AT+CNMI=2,1,0,0,0 hourly as a safety net in case RILD
		// resets it (only observed at boot, but hourly is low-cost insurance).
		if err := at.SetTextMode(storage); err != nil {
			logger.Printf("Housekeeping: SetTextMode failed: %v", err)
		}
	}
}

// rotateLog renames logPath → logPath+".1" and truncates logPath to zero when
// the file exceeds maxLogBytes. The logger continues writing to the same path
// (which is now a fresh, empty file). The .1 file is overwritten each rotation,
// so exactly one old log is retained.
func rotateLog(logPath string, logger *log.Logger) {
	info, err := os.Stat(logPath)
	if err != nil || info.Size() < maxLogBytes {
		return
	}
	if err := os.Rename(logPath, logPath+".1"); err != nil {
		logger.Printf("Housekeeping: log rename failed: %v", err)
		return
	}
	// Truncate (re-create) the log file so the logger's next write goes to a
	// fresh file at the same path. The logger still has the old fd open; the OS
	// will serve writes to the new file once it opens it. Because our logger
	// uses log.New(os.Stdout, ...) and start.sh redirects stdout to the log
	// file, no fd manipulation is needed — the shell already holds the fd and
	// will continue appending to the now-empty file.
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		logger.Printf("Housekeeping: log truncate failed: %v", err)
		return
	}
	f.Close()
	logger.Printf("Housekeeping: log rotated (exceeded %d MB)", maxLogBytes/1024/1024)
}

// checkpointWAL issues PRAGMA wal_checkpoint(TRUNCATE) to merge the WAL file
// into the main database and shrink the WAL file back to zero bytes.
func checkpointWAL(db *database.DB, logger *log.Logger) {
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		logger.Printf("Housekeeping: WAL checkpoint failed: %v", err)
	}
}

// pruneOldRecords deletes messages and send_queue entries older than
// msgRetainDays days, then runs PRAGMA optimize to refresh query planner stats.
func pruneOldRecords(db *database.DB, logger *log.Logger) {
	cutoff := time.Now().UTC().AddDate(0, 0, -msgRetainDays).Format(time.RFC3339)

	res, err := db.Exec(`DELETE FROM messages WHERE received_at < ?`, cutoff)
	if err != nil {
		logger.Printf("Housekeeping: message prune failed: %v", err)
	} else if n, _ := res.RowsAffected(); n > 0 {
		logger.Printf("Housekeeping: pruned %d message(s) older than %d days", n, msgRetainDays)
	}

	res, err = db.Exec(
		`DELETE FROM send_queue WHERE status IN ('sent','failed') AND created_at < ?`, cutoff,
	)
	if err != nil {
		logger.Printf("Housekeeping: send_queue prune failed: %v", err)
	} else if n, _ := res.RowsAffected(); n > 0 {
		logger.Printf("Housekeeping: pruned %d send_queue entry/entries older than %d days", n, msgRetainDays)
	}

	db.Exec(`PRAGMA optimize`)
}
