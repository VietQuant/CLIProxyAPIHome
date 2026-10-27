// Command billing-backfill reprices billing charges that were written as
// default:zero before a model had a price. It uses Home's own pricing code.
//
// It only opens a database connection: it does not run migrations and does not
// register a Home node, so it is safe to point at a live cluster database.
// Without -apply it only reports. Connection settings come from PGHOST, PGPORT,
// PGUSER, PGPASSWORD, PGDATABASE and PGSSLMODE.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"
	// The per-day report needs a timezone database even on minimal images.
	_ "time/tzdata"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func main() {
	if errRun := run(); errRun != nil {
		fmt.Fprintln(os.Stderr, "billing-backfill:", errRun)
		os.Exit(1)
	}
}

func run() error {
	fromFlag := flag.String("from", "", "first charge created_at to reprice, RFC3339 (inclusive)")
	toFlag := flag.String("to", "", "charge created_at upper bound, RFC3339 (exclusive)")
	batch := flag.Int("batch", 20000, "charges per transaction")
	apply := flag.Bool("apply", false, "write the new prices; without it the run is a dry run")
	backupPath := flag.String("backup", "", "file that receives the original values of every changed charge (required with -apply)")
	timezone := flag.String("timezone", "Asia/Ho_Chi_Minh", "timezone for the per-day report")
	verifyLimit := flag.Int("verify", 0, "instead of backfilling, reprice this many charges Home already priced in [from, to) and compare")
	flag.Parse()

	from, errFrom := time.Parse(time.RFC3339, *fromFlag)
	if errFrom != nil {
		return fmt.Errorf("-from: %w", errFrom)
	}
	to, errTo := time.Parse(time.RFC3339, *toFlag)
	if errTo != nil {
		return fmt.Errorf("-to: %w", errTo)
	}
	location, errLocation := time.LoadLocation(*timezone)
	if errLocation != nil {
		return fmt.Errorf("-timezone: %w", errLocation)
	}

	port, _ := strconv.Atoi(os.Getenv("PGPORT"))
	cfg := cluster.PGSQLConfig{
		Host:     os.Getenv("PGHOST"),
		Port:     port,
		User:     os.Getenv("PGUSER"),
		Password: os.Getenv("PGPASSWORD"),
		Database: os.Getenv("PGDATABASE"),
		SSLMode:  os.Getenv("PGSSLMODE"),
	}
	ctx := context.Background()
	db, errOpen := cluster.Open(ctx, cfg)
	if errOpen != nil {
		return fmt.Errorf("open database: %w", errOpen)
	}
	repo := cluster.NewRepository(db)
	out := json.NewEncoder(os.Stdout)
	out.SetIndent("", "  ")

	if *verifyLimit > 0 {
		check, errCheck := repo.VerifyBillingChargeRepricing(ctx, from, to, *verifyLimit)
		if errCheck != nil {
			return errCheck
		}
		return out.Encode(check)
	}

	opts := cluster.BillingZeroChargeBackfillOptions{From: from, To: to, BatchSize: *batch, Apply: *apply, Location: location}
	if *apply {
		if *backupPath == "" {
			return fmt.Errorf("-apply needs -backup")
		}
		backup, errBackup := os.OpenFile(*backupPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errBackup != nil {
			return fmt.Errorf("create backup: %w", errBackup)
		}
		defer func() {
			if errClose := backup.Close(); errClose != nil {
				fmt.Fprintln(os.Stderr, "close backup:", errClose)
			}
		}()
		opts.Backup = backup
	}
	opts.OnBatch = func(progress cluster.BillingZeroChargeBackfillResult) {
		fmt.Fprintf(os.Stderr, "%s scanned=%d priced=%d still_unpriced=%d updated=%d\n", time.Now().UTC().Format(time.RFC3339), progress.Scanned, progress.Priced, progress.StillUnpriced, progress.Updated)
	}
	result, errBackfill := repo.BackfillZeroBillingCharges(ctx, opts)
	if errEncode := out.Encode(result); errEncode != nil {
		return errEncode
	}
	return errBackfill
}
