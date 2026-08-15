package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/dest"
	"github.com/unifers/feather/internal/mongox"
	"github.com/unifers/feather/internal/store"
)

// --- MongoDB accounts ------------------------------------------------------

func (s *Server) handleMongoList(w http.ResponseWriter, r *http.Request) {
	sources, err := s.app.Store.ListMongoSources(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	jobs, err := s.app.Store.ListJobs(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// Which configs read from each account, so an operator can see what a
	// change here would affect before making it.
	usedBy := map[string][]string{}
	for _, job := range jobs {
		alias := job.Config.Source.MongoSource
		usedBy[alias] = append(usedBy[alias], job.Config.Name)
	}

	s.render(w, r, "mongo_list", pageData{
		Title: "MongoDB accounts",
		Nav:   "mongo",
		Data:  map[string]any{"Sources": sources, "UsedBy": usedBy},
	})
}

func (s *Server) handleMongoForm(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"Source": store.MongoSourceRecord{Enabled: true, AuthSource: "admin", AppName: "feather"}}
	title := "Add a MongoDB account"

	if id := r.PathValue("id"); id != "" {
		record, err := s.app.Store.MongoSourceByID(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				s.notFound(w, r)
				return
			}
			s.serverError(w, r, err)
			return
		}
		data["Source"] = record
		title = "MongoDB account: " + record.Alias
	}

	s.render(w, r, "mongo_form", pageData{Title: title, Nav: "mongo", Data: data})
}

func (s *Server) handleMongoSave(w http.ResponseWriter, r *http.Request) {
	f, err := newForm(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	record := store.MongoSourceRecord{
		ID:         r.PathValue("id"),
		Alias:      f.Require("alias"),
		URI:        f.String("uri"),
		AuthSource: f.String("authSource"),
		AppName:    f.String("appName"),
		Enabled:    f.Bool("enabled"),
		Note:       f.String("note"),
	}

	// A connection string that is obviously not one fails here rather than at
	// 2am on the first scheduled run.
	if record.URI != "" && !strings.HasPrefix(record.URI, "mongodb://") && !strings.HasPrefix(record.URI, "mongodb+srv://") {
		f.fail("The connection string must start with mongodb:// or mongodb+srv://")
	}
	if !f.OK() {
		s.setFlash(w, "error", "%s", f.Error())
		s.redirectBack(w, r, mongoFormPath(record.ID))
		return
	}

	if err := s.app.Store.SaveMongoSource(r.Context(), &record); err != nil {
		s.setFlash(w, "error", "%s", err.Error())
		s.redirectBack(w, r, mongoFormPath(record.ID))
		return
	}
	if err := s.reload(r); err != nil {
		s.log.Warn("saved, but could not reload configuration", "error", err)
	}

	s.audit(r, store.AuditSourceSaved, "mongo_source", record.Alias, "")
	s.setFlash(w, "ok", "Saved the MongoDB account %q. Test it to confirm feather can reach it.", record.Alias)
	http.Redirect(w, r, "/mongo", http.StatusSeeOther)
}

func (s *Server) handleMongoTest(w http.ResponseWriter, r *http.Request) {
	record, err := s.app.Store.MongoSourceByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFoundOrError(w, r, err)
		return
	}

	checkErr := mongox.Ping(r.Context(), record.Source())
	if recordErr := s.app.Store.RecordMongoCheck(r.Context(), record.ID, checkErr); recordErr != nil {
		s.log.Warn("could not record connection test", "error", recordErr)
	}
	if checkErr != nil {
		s.setFlash(w, "error", "%s could not be reached: %s", record.Alias, config.RedactSecretsFromError(checkErr))
	} else {
		s.setFlash(w, "ok", "Connected to %s.", record.Alias)
	}
	s.redirectBack(w, r, "/mongo")
}

func (s *Server) handleMongoDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.app.Store.DeleteMongoSource(r.Context(), r.PathValue("id")); err != nil {
		var inUse *store.ErrInUse
		if errors.As(err, &inUse) {
			s.setFlash(w, "error", "%s", inUse.Error())
		} else {
			s.setFlash(w, "error", "%s", err.Error())
		}
		http.Redirect(w, r, "/mongo", http.StatusSeeOther)
		return
	}
	if err := s.reload(r); err != nil {
		s.log.Warn("deleted, but could not reload configuration", "error", err)
	}

	s.audit(r, store.AuditSourceDeleted, "mongo_source", r.PathValue("id"), "")
	s.setFlash(w, "ok", "Removed the MongoDB account.")
	http.Redirect(w, r, "/mongo", http.StatusSeeOther)
}

func mongoFormPath(id string) string {
	if id == "" {
		return "/mongo/new"
	}
	return "/mongo/" + id
}

// --- backup destinations ---------------------------------------------------

func (s *Server) handleDestinationList(w http.ResponseWriter, r *http.Request) {
	destinations, err := s.app.Store.ListDestinations(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	jobs, err := s.app.Store.ListJobs(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	usage, err := s.app.Store.StorageByDestination(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	usedBy := map[string][]string{}
	for _, job := range jobs {
		for _, alias := range job.Config.Destinations {
			usedBy[alias] = append(usedBy[alias], job.Config.Name)
		}
	}
	byAlias := map[string]store.DestinationUsage{}
	for _, u := range usage {
		byAlias[u.Alias] = u
	}

	s.render(w, r, "destination_list", pageData{
		Title: "Backup destinations",
		Nav:   "destinations",
		Data:  map[string]any{"Destinations": destinations, "UsedBy": usedBy, "Usage": byAlias},
	})
}

func (s *Server) handleDestinationForm(w http.ResponseWriter, r *http.Request) {
	record := store.DestinationRecord{Enabled: true}
	record.Provider = config.ProviderS3
	title := "Add a backup destination"

	if id := r.PathValue("id"); id != "" {
		found, err := s.app.Store.DestinationByID(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				s.notFound(w, r)
				return
			}
			s.serverError(w, r, err)
			return
		}
		record = found
		title = "Destination: " + record.Alias
	}

	s.render(w, r, "destination_form", pageData{
		Title: title,
		Nav:   "destinations",
		Data: map[string]any{
			"Destination":    record,
			"StorageClasses": []string{"", "STANDARD", "STANDARD_IA", "INTELLIGENT_TIERING", "GLACIER_IR", "GLACIER", "DEEP_ARCHIVE"},
			// The address Google has to be told to redirect back to. It is
			// shown on the form because it must match the OAuth client exactly.
			"GDriveRedirectURL": s.gdriveRedirectURL(r),
		},
	})
}

func (s *Server) handleDestinationSave(w http.ResponseWriter, r *http.Request) {
	f, err := newForm(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	record := store.DestinationRecord{
		ID:      r.PathValue("id"),
		Enabled: f.Bool("enabled"),
		Note:    f.String("note"),
		Destination: config.Destination{
			Alias:    f.Require("alias"),
			Provider: config.Provider(f.String("provider")),
			Prefix:   strings.Trim(f.String("prefix"), "/"),
		},
	}

	// The two providers share almost no fields, so each reads only its own —
	// otherwise switching a destination to Drive would carry a bucket with it.
	if record.Provider == config.ProviderGDrive {
		record.DriveFolderID = driveFolderID(f.String("driveFolderId"))
		record.DriveID = f.String("driveId")
		record.DriveSubject = f.String("driveSubject")
		record.OAuthClientID = f.String("oauthClientId")
		record.OAuthClientSecret = f.String("oauthClientSecret")
		record.ServiceAccountJSON = strings.TrimSpace(f.Raw("serviceAccountJson"))
	} else {
		record.Region = f.Require("region")
		record.Bucket = f.Require("bucket")
		record.Endpoint = strings.TrimSuffix(f.String("endpoint"), "/")
		record.AccessKeyID = f.String("accessKeyId")
		record.SecretAccessKey = f.String("secretAccessKey")
		record.SessionToken = f.String("sessionToken")
		record.StorageClass = f.String("storageClass")
		record.ForcePathStyle = f.Bool("forcePathStyle")

		// On an edit, a blank secret means "leave the stored one alone" — but a
		// new account with a custom endpoint has nothing to fall back on.
		if record.ID == "" && record.Endpoint != "" && record.SecretAccessKey == "" {
			f.fail("A secret access key is required for a destination with a custom endpoint.")
		}
	}
	if !f.OK() {
		s.setFlash(w, "error", "%s", f.Error())
		s.redirectBack(w, r, destinationFormPath(record.ID))
		return
	}

	if err := s.app.Store.SaveDestination(r.Context(), &record); err != nil {
		s.setFlash(w, "error", "%s", err.Error())
		s.redirectBack(w, r, destinationFormPath(record.ID))
		return
	}
	if err := s.reload(r); err != nil {
		s.log.Warn("saved, but could not reload configuration", "error", err)
	}

	s.audit(r, store.AuditDestSaved, "destination", record.Alias, string(record.Provider))

	// A Drive account with an OAuth client is stored before it is authorised, so
	// the next step is the connect button rather than the destination list.
	if record.Provider == config.ProviderGDrive && !record.Ready() {
		s.setFlash(w, "ok", "Saved %q. Connect it to Google Drive to finish.", record.Alias)
		http.Redirect(w, r, destinationFormPath(record.ID), http.StatusSeeOther)
		return
	}

	s.setFlash(w, "ok", "Saved the destination %q. Test it to confirm the %s is reachable.",
		record.Alias, container(record.Provider))
	http.Redirect(w, r, "/destinations", http.StatusSeeOther)
}

// container is what this provider calls the thing archives are written into.
func container(provider config.Provider) string {
	if provider == config.ProviderGDrive {
		return "folder"
	}
	return "bucket"
}

func (s *Server) handleDestinationTest(w http.ResponseWriter, r *http.Request) {
	record, err := s.app.Store.DestinationByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFoundOrError(w, r, err)
		return
	}

	if !record.Ready() {
		s.setFlash(w, "error", "%s is not connected to Google Drive yet, so there is nothing to test.", record.Alias)
		s.redirectBack(w, r, "/destinations")
		return
	}

	checkErr := dest.Probe(r.Context(), record.Destination)
	if recordErr := s.app.Store.RecordDestinationCheck(r.Context(), record.ID, checkErr); recordErr != nil {
		s.log.Warn("could not record destination test", "error", recordErr)
	}
	switch {
	case checkErr != nil:
		s.setFlash(w, "error", "%s", config.RedactSecretsFromError(checkErr))
	case record.Provider == config.ProviderGDrive:
		s.setFlash(w, "ok", "Reached Drive folder %s on %s.", record.DriveRoot(), record.Alias)
	default:
		s.setFlash(w, "ok", "Reached bucket %s on %s.", record.Bucket, record.Alias)
	}
	s.redirectBack(w, r, "/destinations")
}

func (s *Server) handleDestinationDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.app.Store.DeleteDestination(r.Context(), r.PathValue("id")); err != nil {
		var inUse *store.ErrInUse
		if errors.As(err, &inUse) {
			s.setFlash(w, "error", "%s", inUse.Error())
		} else {
			s.setFlash(w, "error", "%s", err.Error())
		}
		http.Redirect(w, r, "/destinations", http.StatusSeeOther)
		return
	}
	if err := s.reload(r); err != nil {
		s.log.Warn("deleted, but could not reload configuration", "error", err)
	}

	s.audit(r, store.AuditDestDeleted, "destination", r.PathValue("id"), "archives already written stay listed")
	s.setFlash(w, "ok", "Removed the destination. Archives already written to it stay listed under Backups.")
	http.Redirect(w, r, "/destinations", http.StatusSeeOther)
}

func destinationFormPath(id string) string {
	if id == "" {
		return "/destinations/new"
	}
	return "/destinations/" + id
}

// redirectBack returns to the page the form was posted from, falling back to a
// sensible page when there is no referer to trust.
func (s *Server) redirectBack(w http.ResponseWriter, r *http.Request, fallback string) {
	if referer := r.Referer(); referer != "" {
		if parsed, err := parseSameSite(referer, r); err == nil {
			http.Redirect(w, r, parsed, http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, fallback, http.StatusSeeOther)
}
