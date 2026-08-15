package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	// Schedules carry a timezone, and JobConfig.Prepare resolves it with
	// time.LoadLocation on every boot for every config. Without the system
	// tzdata that call fails, so a `make install` onto a minimal host turned
	// every "Asia/Kolkata" schedule into a parked config. Embedding the
	// database costs about 450 KB in the binary and removes the whole class:
	// the system copy is still preferred when there is one.
	_ "time/tzdata"

	"golang.org/x/term"

	"github.com/unifers/feather/internal/app"
	"github.com/unifers/feather/internal/archive"
	"github.com/unifers/feather/internal/buildinfo"
	"github.com/unifers/feather/internal/bundle"
	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/logging"
	"github.com/unifers/feather/internal/mongox"
	"github.com/unifers/feather/internal/secrets"
	"github.com/unifers/feather/internal/store"
	"github.com/unifers/feather/internal/web"
)

const usage = `feather — MongoDB retention, archival and deletion

Everything is configured in the web UI and stored in feather's own database:
the administrator login, MongoDB accounts, S3, Oracle and Google Drive
destinations, and the retention configs.

Usage:
  feather serve                  Run the control panel and the scheduler
  feather daemon                 Run the scheduler only, with no web UI
  feather run <config> [--dry-run]
                                 Run one retention config immediately
  feather validate               Check accounts, configs and connectivity
  feather list                   Show configs, schedules and last run status
  feather export <file>          Write every account, config and setting to one bundle
  feather import-bundle <file>   Read a bundle written by "feather export" back in
  feather import                 Import accounts and configs from .env and JSON files
  feather create-admin <email>   Add an administrator
  feather reset-password <email> Set an administrator's password
  feather keygen                 Print a master key for FEATHER_MASTER_KEY
  feather migrate                Apply the database schema and exit
  feather version                Print the version, commit and build date

Flags:
  --env <path>       Env file to read (default: .env in the working directory)
  --addr <host:port> Override FEATHER_HTTP_ADDR (default 127.0.0.1:8477)
  --data-dir <path>  Override FEATHER_DATA_DIR
  --dry-run          Force a dry run, whatever the config says
  --secure-cookies   Mark session cookies Secure (set this behind HTTPS)
  --password <pass>  Non-interactive password for create-admin/reset-password
  --passphrase <p>   Non-interactive passphrase for export/import-bundle
  --no-secrets       Export without credentials, so no passphrase is needed
  --overwrite        Let import-bundle replace accounts and configs already here
  --skip-settings    Leave this instance's name and concurrency cap alone
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		return errors.New("no command given")
	}

	command := os.Args[1]
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	envFile := flags.String("env", "", "path to env file")
	addr := flags.String("addr", "", "override FEATHER_HTTP_ADDR")
	dataDir := flags.String("data-dir", "", "override FEATHER_DATA_DIR")
	dryRun := flags.Bool("dry-run", false, "force a dry run regardless of the config setting")
	secureCookies := flags.Bool("secure-cookies", false, "mark session cookies Secure")
	password := flags.String("password", "", "password for create-admin/reset-password")
	importEnv := flags.String("from-env", ".env", "env file to import accounts from")
	importConfigs := flags.String("from-configs", "./configs", "directory of JSON retention configs to import")
	passphrase := flags.String("passphrase", "", "passphrase for export/import-bundle")
	noSecrets := flags.Bool("no-secrets", false, "export without credentials")
	overwrite := flags.Bool("overwrite", false, "replace entries that already exist")
	skipSettings := flags.Bool("skip-settings", false, "do not apply instance settings from the bundle")

	args := os.Args[2:]
	// Several commands take a positional argument before their flags.
	var positional string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional, args = args[0], args[1:]
	}
	if err := flags.Parse(args); err != nil {
		return err
	}

	if command == "keygen" {
		key, err := secrets.GenerateKeyHex()
		if err != nil {
			return err
		}
		fmt.Println(key)
		return nil
	}
	if command == "-h" || command == "--help" || command == "help" {
		fmt.Print(usage)
		return nil
	}
	// Answered before the env file is read: "what is running here?" must be
	// answerable on a box whose configuration is the thing that is broken.
	if command == "version" || command == "--version" || command == "-v" {
		fmt.Println(buildinfo.String())
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	env, err := config.LoadEnv(*envFile)
	if err != nil {
		return err
	}
	if *dataDir != "" {
		env.DataDir = *dataDir
		env.DBPath = *dataDir + "/feather.db"
		env.MasterKeyPath = *dataDir + "/master.key"
		env.SpoolDir = *dataDir + "/spool"
	}
	if *addr != "" {
		env.HTTPAddr = *addr
	}
	log := logging.Init(env.LogLevel, env.LogJSON)

	switch command {
	case "serve":
		return runServe(ctx, env, log, *secureCookies)
	case "daemon":
		return runDaemon(ctx, env, log)
	case "run":
		if positional == "" {
			return errors.New("usage: feather run <config> [--dry-run]")
		}
		return runOnce(ctx, env, log, positional, *dryRun)
	case "validate":
		return runValidate(ctx, env, log)
	case "list":
		return runList(ctx, env, log)
	case "migrate":
		return runMigrate(ctx, env, log)
	case "import":
		return runImport(ctx, env, log, *importEnv, *importConfigs)
	case "export":
		if positional == "" {
			return errors.New("usage: feather export <file> [--passphrase <passphrase>] [--no-secrets]")
		}
		return runExport(ctx, env, log, positional, *passphrase, *noSecrets)
	case "import-bundle":
		if positional == "" {
			return errors.New("usage: feather import-bundle <file> [--passphrase <passphrase>] [--overwrite]")
		}
		return runImportBundle(ctx, env, log, positional, *passphrase, *overwrite, !*skipSettings)
	case "create-admin":
		return runCreateAdmin(ctx, env, log, positional, *password)
	case "reset-password":
		return runResetPassword(ctx, env, log, positional, *password)
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", command)
	}
}

// runServe is the normal way to run feather: the control panel and the
// scheduler in one process, so a config saved in the browser is live at once.
func runServe(ctx context.Context, env *config.Env, log *slog.Logger, secureCookies bool) error {
	a, err := app.Open(ctx, env, log)
	if err != nil {
		return err
	}
	defer a.Close(ctx)

	a.Housekeeping(ctx)
	if removed, err := archive.CleanSpoolDir(env.SpoolDir); err != nil {
		log.Warn("could not clean spool dir", "error", err)
	} else if removed > 0 {
		log.Info("removed orphaned spool files", "count", removed)
	}

	needsSetup, err := a.Store.NeedsSetup(ctx)
	if err != nil {
		return err
	}
	if needsSetup {
		log.Info("no administrator yet — open the control panel to create one", "url", displayURL(env))
	}

	server, err := web.New(a, log, secureCookies)
	if err != nil {
		return err
	}

	a.Scheduler.Start()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		a.Scheduler.Stop(shutdownCtx)
	}()

	log.Info("feather started", "version", buildinfo.Version(), "commit", buildinfo.Get().Commit,
		"url", displayURL(env), "spool", env.SpoolDir)
	if err := server.Serve(ctx, env.HTTPAddr); err != nil {
		return err
	}

	log.Info("shutdown signal received, waiting for in-flight runs")
	return nil
}

// runDaemon runs the schedule with no control panel, for an installation
// configured elsewhere and locked down.
func runDaemon(ctx context.Context, env *config.Env, log *slog.Logger) error {
	a, err := app.Open(ctx, env, log)
	if err != nil {
		return err
	}
	defer a.Close(ctx)

	a.Housekeeping(ctx)
	if removed, err := archive.CleanSpoolDir(env.SpoolDir); err != nil {
		log.Warn("could not clean spool dir", "error", err)
	} else if removed > 0 {
		log.Info("removed orphaned spool files", "count", removed)
	}
	if err := a.Registry.CheckAll(ctx); err != nil {
		return err
	}

	a.Scheduler.Start()
	log.Info("feather daemon started", "version", buildinfo.Version(), "commit", buildinfo.Get().Commit,
		"max_concurrent", a.MaxConcurrentJobs(ctx))

	<-ctx.Done()
	log.Info("shutdown signal received, waiting for in-flight runs")

	// Generous but bounded: an in-flight batch should be allowed to finish
	// uploading and record itself rather than be cut off mid-catalogue.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	a.Scheduler.Stop(shutdownCtx)

	log.Info("feather daemon stopped")
	return nil
}

func runOnce(ctx context.Context, env *config.Env, log *slog.Logger, name string, forceDryRun bool) error {
	a, err := app.Open(ctx, env, log)
	if err != nil {
		return err
	}
	defer a.Close(ctx)

	record, err := a.Store.JobByName(ctx, name)
	if err != nil {
		return err
	}
	if forceDryRun {
		record.Config.DryRun = true
	}
	if err := a.Registry.CheckAll(ctx); err != nil {
		return err
	}

	_, err = a.Runner.Run(ctx, record.Config, store.TriggerManual)
	return err
}

func runValidate(ctx context.Context, env *config.Env, log *slog.Logger) error {
	a, err := app.Open(ctx, env, log)
	if err != nil {
		return err
	}
	defer a.Close(ctx)

	if err := a.Registry.CheckAll(ctx); err != nil {
		return err
	}
	log.Info("destinations reachable", "aliases", a.Registry.Aliases())

	sources, err := a.Store.ListMongoSources(ctx)
	if err != nil {
		return err
	}
	for _, source := range sources {
		if !source.Enabled {
			continue
		}
		if err := mongox.Ping(ctx, source.Source()); err != nil {
			return fmt.Errorf("MongoDB account %q: %w", source.Alias, err)
		}
	}
	log.Info("MongoDB accounts reachable", "count", len(sources))

	jobs, err := a.Store.Jobs(ctx)
	if err != nil {
		return err
	}

	now := time.Now()
	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "CONFIG\tCOLLECTION\tMATCHING\tDELETE\tDESTINATIONS\tSCHEDULE\tRESOLVED FILTER")

	for _, job := range jobs {
		resolved, err := config.ResolveTimeTokens(job.Source.Filter, now)
		if err != nil {
			return fmt.Errorf("config %s: %w", job.Name, err)
		}
		filter, err := mongox.ParseFilter(resolved)
		if err != nil {
			return fmt.Errorf("config %s: %w", job.Name, err)
		}

		client, err := a.Mongo.Get(ctx, job.Source.MongoSource)
		if err != nil {
			return fmt.Errorf("config %s: %w", job.Name, err)
		}
		coll, err := client.Collection(job.Source.Database, job.Source.Collection, job.Source.ReadPreference)
		if err != nil {
			return fmt.Errorf("config %s: %w", job.Name, err)
		}
		matching, err := mongox.Count(ctx, coll, filter)
		if err != nil {
			return fmt.Errorf("config %s: counting matching documents: %w", job.Name, err)
		}

		fmt.Fprintf(writer, "%s\t%s.%s\t%d\t%s\t%v\t%s\t%s\n",
			job.Name, job.Source.Database, job.Source.Collection, matching,
			yesNo(job.Retention.DeleteAfterArchive), job.Destinations,
			scheduleSummary(job), compactJSON(resolved))
	}
	if err := writer.Flush(); err != nil {
		return err
	}

	log.Info("all configs valid", "count", len(jobs))
	return nil
}

func runList(ctx context.Context, env *config.Env, log *slog.Logger) error {
	a, err := app.Open(ctx, env, log)
	if err != nil {
		return err
	}
	defer a.Close(ctx)

	jobs, err := a.Store.Jobs(ctx)
	if err != nil {
		return err
	}
	latest, err := a.Store.LatestRuns(ctx)
	if err != nil {
		return err
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "CONFIG\tENABLED\tDELETES\tSCHEDULE\tLAST RUN\tSTATUS\tARCHIVED\tDELETED\tSTORED")

	for _, job := range jobs {
		lastRun, status, archived, deleted, stored := "never", "-", "-", "-", "-"
		if summary, ok := latest[job.Name]; ok {
			lastRun = summary.StartedAt.Local().Format("2006-01-02 15:04")
			status = string(summary.Status)
			archived = fmt.Sprint(summary.DocsArchived)
			deleted = fmt.Sprint(summary.DocsDeleted)
			stored = config.HumanBytes(summary.BytesStored)
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			job.Name, yesNo(job.IsEnabled()), yesNo(job.Retention.DeleteAfterArchive),
			scheduleSummary(job), lastRun, status, archived, deleted, stored)
	}
	return writer.Flush()
}

func runMigrate(ctx context.Context, env *config.Env, log *slog.Logger) error {
	a, err := app.Open(ctx, env, log)
	if err != nil {
		return err
	}
	defer a.Close(ctx)

	log.Info("database schema is up to date", "path", a.Store.Path())
	return nil
}

// runImport moves an older file-based installation into the database: the
// MongoDB account and destinations from .env, and the retention configs from
// the JSON directory. Existing entries are left alone, so it is safe to repeat.
func runImport(ctx context.Context, env *config.Env, log *slog.Logger, envPath, configDir string) error {
	a, err := app.Open(ctx, env, log)
	if err != nil {
		return err
	}
	defer a.Close(ctx)

	legacy, err := config.LoadLegacyEnv(envPath)
	if err != nil {
		return err
	}

	existingSources, err := a.Store.MongoSourceMap(ctx)
	if err != nil {
		return err
	}
	var importedSources int
	for alias, source := range legacy.Sources {
		if _, exists := existingSources[alias]; exists {
			log.Info("MongoDB account already present, skipping", "alias", alias)
			continue
		}
		record := store.MongoSourceRecord{
			Alias:      alias,
			URI:        source.URI,
			AuthSource: source.AuthSource,
			AppName:    source.AppName,
			Enabled:    true,
			Note:       "imported from " + envPath,
		}
		if err := a.Store.SaveMongoSource(ctx, &record); err != nil {
			return fmt.Errorf("importing MongoDB account %q: %w", alias, err)
		}
		importedSources++
	}

	existingDestinations, err := a.Store.DestinationMap(ctx)
	if err != nil {
		return err
	}
	var importedDestinations int
	for alias, destination := range legacy.Destinations {
		if _, exists := existingDestinations[alias]; exists {
			log.Info("destination already present, skipping", "alias", alias)
			continue
		}
		record := store.DestinationRecord{
			Enabled:     true,
			Note:        "imported from " + envPath,
			Destination: destination,
		}
		if err := a.Store.SaveDestination(ctx, &record); err != nil {
			return fmt.Errorf("importing destination %q: %w", alias, err)
		}
		importedDestinations++
	}

	// Reload so the configs about to be imported can be validated against the
	// accounts that just arrived.
	catalog, err := a.Catalog(ctx)
	if err != nil {
		return err
	}

	var importedJobs int
	if entries, statErr := os.Stat(configDir); statErr == nil && entries.IsDir() {
		// The old format had no per-config MongoDB account: there was only one,
		// so every imported config points at it.
		defaultSource := soleAlias(catalog.Sources)
		jobs, err := config.LoadJobsLenient(configDir, catalog, defaultSource)
		if err != nil {
			return err
		}
		for _, job := range jobs {
			if _, err := a.Store.JobByName(ctx, job.Name); err == nil {
				log.Info("retention config already present, skipping", "name", job.Name)
				continue
			}
			record := store.JobRecord{Config: job}
			if err := a.Store.SaveJob(ctx, &record); err != nil {
				return fmt.Errorf("importing config %q: %w", job.Name, err)
			}
			importedJobs++
		}
	} else {
		log.Info("no config directory to import", "path", configDir)
	}

	log.Info("import complete",
		"mongo_accounts", importedSources,
		"destinations", importedDestinations,
		"configs", importedJobs)
	fmt.Printf("Imported %d MongoDB account(s), %d destination(s), %d retention config(s) into %s.\n",
		importedSources, importedDestinations, importedJobs, a.Store.Path())
	fmt.Println("Credentials in", envPath, "are no longer read; you can remove them once you have confirmed the import.")
	return nil
}

// runExport writes the whole installation — accounts, destinations, retention
// configs and settings — to one file, credentials re-sealed under a passphrase
// the operator supplies. The master key stays where it is.
func runExport(ctx context.Context, env *config.Env, log *slog.Logger, path, passphrase string, noSecrets bool) error {
	a, err := app.Open(ctx, env, log)
	if err != nil {
		return err
	}
	defer a.Close(ctx)

	if !noSecrets && passphrase == "" {
		if passphrase, err = promptPassword("Passphrase to seal the credentials with: "); err != nil {
			return err
		}
		confirm, err := promptPassword("Confirm passphrase: ")
		if err != nil {
			return err
		}
		if passphrase != confirm {
			return errors.New("the two passphrases do not match")
		}
	}

	exported, err := bundle.Export(ctx, a, bundle.ExportOptions{
		Passphrase:     passphrase,
		WithoutSecrets: noSecrets,
	})
	if err != nil {
		return err
	}
	body, err := exported.Encode()
	if err != nil {
		return err
	}

	// O_EXCL: an export is easy to repeat, and silently replacing yesterday's
	// bundle with today's is not something to discover later.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s already exists — remove it or choose another name", path)
		}
		return err
	}
	if _, err := file.Write(body); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}

	fmt.Printf("Wrote %s: %d MongoDB account(s), %d destination(s), %d retention config(s).\n",
		path, len(exported.MongoSources), len(exported.Destinations), len(exported.Configs))
	if exported.HasSecrets() {
		fmt.Println("Credentials in it are sealed with the passphrase you typed. Without that passphrase the file cannot be imported.")
	} else {
		fmt.Println("No credentials are in it. Importing it elsewhere will not create accounts that need one.")
	}
	return nil
}

// runImportBundle reads a bundle back in. Without --overwrite it only adds what
// is missing, so it is safe to run against an instance that is already set up.
func runImportBundle(ctx context.Context, env *config.Env, log *slog.Logger, path, passphrase string, overwrite, withSettings bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	incoming, err := bundle.Decode(data)
	if err != nil {
		return err
	}

	a, err := app.Open(ctx, env, log)
	if err != nil {
		return err
	}
	defer a.Close(ctx)

	if incoming.HasSecrets() && passphrase == "" {
		if passphrase, err = promptPassword("Passphrase this bundle was exported with: "); err != nil {
			return err
		}
	}

	result, err := incoming.Apply(ctx, a, bundle.ImportOptions{
		Passphrase: passphrase,
		Overwrite:  overwrite,
		Settings:   withSettings,
	})
	if err != nil {
		return err
	}

	fmt.Println(result.Summary())
	for _, note := range result.Notes {
		fmt.Println("  not applied:", note)
	}
	if result.Applied() == 0 {
		fmt.Println("Nothing was changed.")
		return nil
	}
	fmt.Println("Imported configs decide what is archived and whether the originals are deleted — review them before the next scheduled run.")
	return nil
}

func runCreateAdmin(ctx context.Context, env *config.Env, log *slog.Logger, email, password string) error {
	if email == "" {
		return errors.New("usage: feather create-admin <email> [--password <password>]")
	}
	a, err := app.Open(ctx, env, log)
	if err != nil {
		return err
	}
	defer a.Close(ctx)

	if password == "" {
		password, err = promptPassword("Password: ")
		if err != nil {
			return err
		}
	}
	user, err := a.Store.CreateAdmin(ctx, email, password)
	if err != nil {
		return err
	}

	fmt.Printf("Created administrator %s.\n", user.Email)
	return nil
}

func runResetPassword(ctx context.Context, env *config.Env, log *slog.Logger, email, password string) error {
	if email == "" {
		return errors.New("usage: feather reset-password <email> [--password <password>]")
	}
	a, err := app.Open(ctx, env, log)
	if err != nil {
		return err
	}
	defer a.Close(ctx)

	if password == "" {
		password, err = promptPassword("New password: ")
		if err != nil {
			return err
		}
	}
	if err := a.Store.ResetPassword(ctx, email, password); err != nil {
		return err
	}

	fmt.Printf("Password for %s has been reset. Every existing session was signed out.\n", email)
	return nil
}

// promptPassword reads a password without echoing it. When stdin is not a
// terminal — a provisioning script, say — it reads a line instead.
func promptPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)

	if term.IsTerminal(int(os.Stdin.Fd())) {
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	}

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return "", errors.New("no password supplied on stdin")
	}
	return strings.TrimSpace(scanner.Text()), nil
}

func displayURL(env *config.Env) string {
	if env.BaseURL != "" {
		return env.BaseURL
	}
	addr := env.HTTPAddr
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	return "http://" + addr
}

func soleAlias(sources map[string]config.MongoSource) string {
	if len(sources) != 1 {
		return ""
	}
	for alias := range sources {
		return alias
	}
	return ""
}

func scheduleSummary(job *config.JobConfig) string {
	if job.Schedule.Cron != "" {
		return fmt.Sprintf("cron %s (%s)", job.Schedule.Cron, job.Schedule.Timezone)
	}
	if job.Schedule.AtHour >= 0 {
		return fmt.Sprintf("every %s at %02d:%02d %s", job.Schedule.Every, job.Schedule.AtHour, job.Schedule.AtMinute, job.Schedule.Timezone)
	}
	return "every " + job.Schedule.Every
}

func compactJSON(raw []byte) string {
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return string(raw)
	}
	// Marshalling through a map sorts the keys, so the same filter always reads
	// the same way in validate output.
	compact, err := json.Marshal(out)
	if err != nil {
		return string(raw)
	}
	return string(compact)
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}
