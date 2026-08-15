package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LoadJobs reads every *.json file in dir and validates each against the known
// accounts. All configs are validated before any is returned: a single bad file
// fails the whole load rather than leaving a partially understood state.
//
// Retention configs live in the database now; this is the reader `feather
// import` uses to bring an older file-based installation across.
func LoadJobs(dir string, known Catalog) ([]*JobConfig, error) {
	return loadJobs(dir, known, "", true)
}

// LoadJobsLenient is the importer's reader. It tolerates fields the current
// format no longer knows — an old file naming a since-removed setting should
// still be importable — and fills in the MongoDB account for configs written
// when there could only be one.
func LoadJobsLenient(dir string, known Catalog, defaultSource string) ([]*JobConfig, error) {
	return loadJobs(dir, known, defaultSource, false)
}

func loadJobs(dir string, known Catalog, defaultSource string, strict bool) ([]*JobConfig, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading config dir %s: %w", dir, err)
	}

	jobs := make([]*JobConfig, 0, len(entries))
	seen := map[string]string{}
	var problems []string

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())

		job, err := loadJob(path, known, defaultSource, strict)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if previous, dup := seen[job.Name]; dup {
			problems = append(problems, fmt.Sprintf("%s: name %q is already used by %s", path, job.Name, previous))
			continue
		}
		seen[job.Name] = path
		jobs = append(jobs, job)
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid retention config(s):\n  - %s", strings.Join(problems, "\n  - "))
	}

	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Name < jobs[j].Name })
	return jobs, nil
}

// LoadJob reads and validates a single config file.
func LoadJob(path string, known Catalog) (*JobConfig, error) {
	return loadJob(path, known, "", true)
}

func loadJob(path string, known Catalog, defaultSource string, strict bool) (*JobConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	job := &JobConfig{}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(job); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	if job.Name == "" {
		job.Name = strings.TrimSuffix(filepath.Base(path), ".json")
	}
	if job.Source.MongoSource == "" {
		job.Source.MongoSource = defaultSource
	}
	job.SourcePath = path
	job.SHA256 = job.ComputeSHA()

	if err := job.Validate(known); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return job, nil
}

// FindJob returns the config with the given name.
func FindJob(jobs []*JobConfig, name string) (*JobConfig, error) {
	for _, job := range jobs {
		if job.Name == name {
			return job, nil
		}
	}
	available := make([]string, 0, len(jobs))
	for _, job := range jobs {
		available = append(available, job.Name)
	}
	return nil, fmt.Errorf("no retention config named %q (available: %s)", name, strings.Join(available, ", "))
}
