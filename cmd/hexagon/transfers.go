package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/andrea/hexagon/internal/store"
)

// transferJanitorInterval is how often expired image transfers are swept
// away. Fifteen minutes keeps a multi-gigabyte file from lingering long past
// its lifetime, and costs nothing on a machine that is somebody's laptop.
const transferJanitorInterval = 15 * time.Minute

// failInterruptedTransfers marks transfers that were mid-job when the server
// stopped as failed, and removes the incomplete file each one left behind — a
// row left "running" describes a job nobody is doing.
func failInterruptedTransfers(ctx context.Context, st *store.Store, transfersDir string, log *slog.Logger) {
	interrupted, err := st.FailInterruptedTransfers(ctx)
	if err != nil {
		log.Warn("clear interrupted transfers", "err", err)
		return
	}
	for _, t := range interrupted {
		removeTransferFiles(t.ID, transfersDir, log)
	}
	if len(interrupted) > 0 {
		log.Info("marked interrupted image transfers as failed", "count", len(interrupted))
	}
}

// runTransferJanitor removes every transfer whose lifetime is over, on a
// ticker and once at startup: a backup nobody collected is a multi-gigabyte
// file that would otherwise be permanent. It returns when ctx is done.
func runTransferJanitor(ctx context.Context, st *store.Store, transfersDir string, log *slog.Logger) {
	sweep := func() {
		expired, err := st.ExpiredTransfers(ctx, time.Now())
		if err != nil {
			log.Warn("list expired transfers", "err", err)
			return
		}
		for _, t := range expired {
			if err := st.DeleteTransfer(ctx, t.UserID, t.ID); err != nil {
				log.Warn("delete expired transfer", "id", t.ID, "err", err)
				continue
			}
			removeTransferFiles(t.ID, transfersDir, log)
		}
		if len(expired) > 0 {
			log.Info("removed expired image transfers", "count", len(expired))
		}
	}

	sweep()
	ticker := time.NewTicker(transferJanitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

func removeTransferFiles(id, transfersDir string, log *slog.Logger) {
	if err := os.RemoveAll(filepath.Join(transfersDir, id)); err != nil {
		log.Warn("remove transfer directory", "id", id, "err", err)
	}
}
