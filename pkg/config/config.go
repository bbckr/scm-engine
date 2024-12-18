package config

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hashicorp/go-multierror"
	"github.com/jippi/scm-engine/pkg/scm"
	slogctx "github.com/veqryn/slog-context"
)

type Config struct {
	// (Optional) When on, no actions will be taken, but instead logged for review
	DryRun *bool `json:"dry_run,omitempty" yaml:"dry_run" jsonschema:"default=false"`

	// (Optional) Import configuration from other git repositories
	//
	// See: https://jippi.github.io/scm-engine/configuration/#include
	Includes []Include `json:"include,omitempty" yaml:"include"`

	// (Optional) Configure what users that should be ignored when considering activity on a Merge Request
	//
	// SCM-Engine defines activity as comments, reviews, commits, adding/removing labels and similar actions made on a change request.
	//
	// See: https://jippi.github.io/scm-engine/configuration/#ignore_activity_from
	IgnoreActivityFrom IgnoreActivityFrom `json:"ignore_activity_from,omitempty" yaml:"ignore_activity_from"`

	// (Optional) Actions can modify a Merge Request in various ways, for example, adding a comment or closing the Merge Request.
	//
	// See: https://jippi.github.io/scm-engine/configuration/#actions
	Actions Actions `json:"actions,omitempty" yaml:"actions"`

	// (Optional) Labels are a way to categorize and filter issues, merge requests, and epics in GitLab. -- GitLab documentation
	//
	// See: https://jippi.github.io/scm-engine/configuration/#label
	Labels Labels `json:"label,omitempty" yaml:"label"`
}

func (c Config) Lint(_ context.Context, evalContext scm.EvalContext) error {
	var errors error

	for _, action := range c.Actions {
		if _, err := action.Setup(evalContext); err != nil {
			errors = multierror.Append(errors, fmt.Errorf("Action %q failed validation: %w", action.Name, err))
		}
	}

	for _, label := range c.Labels {
		if err := label.Setup(evalContext); err != nil {
			errors = multierror.Append(errors, fmt.Errorf("Label %q failed validation: %w", label.Name, err))
		}
	}

	return errors
}

func (c Config) Evaluate(ctx context.Context, evalContext scm.EvalContext) ([]scm.EvaluationResult, []Action, error) {
	slogctx.Info(ctx, "Evaluating labels")

	labels, err := c.Labels.Evaluate(ctx, evalContext)
	if err != nil {
		return nil, nil, fmt.Errorf("evaluation failed: %w", err)
	}

	slogctx.Info(ctx, "Evaluating Actions")

	actions, err := c.Actions.Evaluate(ctx, evalContext)
	if err != nil {
		return nil, nil, err
	}

	return labels, actions, nil
}

func (c *Config) LoadIncludes(ctx context.Context, client scm.Client) error {
	// No files to include
	if len(c.Includes) == 0 {
		return nil
	}

	// Update logger with a friendly tag to differentiate the events within
	ctx = slogctx.With(ctx, slog.String("phase", "remote_include"))

	// For each project, do a read of all the files we need
	for _, include := range c.Includes {
		ctx := slogctx.With(ctx, slog.Any("remote_include_config", include))

		slogctx.Debug(ctx, fmt.Sprintf("Loading remote configuration from project %q", include.Project))

		files, err := client.GetProjectFiles(ctx, include.Project, include.Ref, include.Files)
		if err != nil {
			return fmt.Errorf("failed to load included config files from project [%s]: %w", include.Project, err)
		}

		for fileName, fileContent := range files {
			remoteConfig, err := ParseFileString(fileContent)
			if err != nil {
				return fmt.Errorf("failed to parse remote config file [%s] from project [%s]: %w", fileName, include.Project, err)
			}

			// Disallow nested includes
			if len(remoteConfig.Includes) != 0 {
				slogctx.Warn(ctx, fmt.Sprintf("file [%s] from project [%s] may not have any 'include' settings; Recursive include is not supported", fileName, include.Project))
			}

			// Disallow changing dry run
			if remoteConfig.DryRun != nil {
				slogctx.Warn(ctx, fmt.Sprintf("file [%s] from project [%s] may not have a 'dry_run' setting; Remote include are not allowed to change this setting", fileName, include.Project))
			}

			// Append actions
			if len(remoteConfig.Actions) != 0 {
				slogctx.Debug(ctx, fmt.Sprintf("file [%s] from project [%s] added %d new actions to the config file", fileName, include.Project, len(remoteConfig.Actions)))

				c.Actions = append(c.Actions, remoteConfig.Actions...)
			}

			// Append labels
			if len(remoteConfig.Labels) != 0 {
				slogctx.Debug(ctx, fmt.Sprintf("file [%s] from project [%s] added %d new labels to the config file", fileName, include.Project, len(remoteConfig.Labels)))

				c.Labels = append(c.Labels, remoteConfig.Labels...)
			}
		}
	}

	slogctx.Debug(ctx, "Done loading remote configuration files")

	return nil
}

// Merge merges the other config into the current config
func (c *Config) Merge(other *Config) *Config {
	cfg := &Config{}

	if other == nil {
		return &Config{
			DryRun:             c.DryRun,
			IgnoreActivityFrom: c.IgnoreActivityFrom,
			Actions:            c.Actions,
			Labels:             c.Labels,
			Includes:           c.Includes,
		}
	}

	cfg.DryRun = other.DryRun

	cfg.IgnoreActivityFrom.IsBot = other.IgnoreActivityFrom.IsBot

	if c.IgnoreActivityFrom.Usernames != nil || other.IgnoreActivityFrom.Usernames != nil {
		cfg.IgnoreActivityFrom.Usernames = scm.MergeSlices(c.IgnoreActivityFrom.Usernames, other.IgnoreActivityFrom.Usernames, func(username string) string {
			return username
		})
	}

	if c.IgnoreActivityFrom.Emails != nil || other.IgnoreActivityFrom.Emails != nil {
		cfg.IgnoreActivityFrom.Emails = scm.MergeSlices(c.IgnoreActivityFrom.Emails, other.IgnoreActivityFrom.Emails, func(email string) string {
			return email
		})
	}

	if c.Actions != nil || other.Actions != nil {
		cfg.Actions = scm.MergeSlices(c.Actions, other.Actions, func(action Action) string {
			return action.Name
		})
	}

	if c.Labels != nil || other.Labels != nil {
		cfg.Labels = scm.MergeSlices(c.Labels, other.Labels, func(label *Label) string {
			return label.Name
		})
	}

	// Merge includes, but skip adding duplicate files under a project/ref
	// The ordering of the Includes matters, so preserve the ordering where
	// the c.Includes will be evaluated before the other.Includes in their
	// set order.
	//
	// The end result decomposes the includes to a single include per project/ref
	// and file, rather than joining files to a single project/ref.
	if c.Includes != nil || other.Includes != nil {
		includesMap := map[string]map[string]*bool{} // to check if a file is already included
		includesPos := map[string]int{}              // to know where to insert new includes

		mergedIncludes := make([]Include, 0, len(c.Includes)+len(other.Includes))

		for _, include := range c.Includes {
			for _, file := range include.Files {
				// get the position of the project/ref in the mergedIncludes slice
				pos, ok := includesPos[key(include.Project, include.Ref)]
				if !ok {
					// when it is not found we want to stick any new project/ref includes at the end
					// of the mergedIncludes slice
					includesMap[key(include.Project, include.Ref)] = map[string]*bool{}

					pos = len(mergedIncludes) // start from the end of the mergedIncludes slice
					includesPos[key(include.Project, include.Ref)] = pos

					mergedIncludes = append(mergedIncludes, Include{
						Project: include.Project,
						Ref:     include.Ref,
						Files:   []string{},
					})
				}

				// if the file is not already included, add it to the includesMap
				// and add it to the mergedIncludes slice at the correct position
				if _, ok := includesMap[key(include.Project, include.Ref)][file]; !ok {
					includesMap[key(include.Project, include.Ref)][file] = nil // track that the file is included

					mergedIncludes[pos].Files = append(mergedIncludes[pos].Files, file)
				}
			}
		}

		for _, include := range other.Includes {
			for _, file := range include.Files {
				// get the position of the project/ref in the mergedIncludes slice
				pos, ok := includesPos[key(include.Project, include.Ref)]
				if !ok {
					// when it is not found we want to stick any new project/ref includes at the end
					// of the mergedIncludes slice
					includesMap[key(include.Project, include.Ref)] = map[string]*bool{}

					pos = len(mergedIncludes) // start from the end of the mergedIncludes slice
					includesPos[key(include.Project, include.Ref)] = pos

					mergedIncludes = append(mergedIncludes, Include{
						Project: include.Project,
						Ref:     include.Ref,
						Files:   []string{},
					})
				}

				// if the file is not already included, add it to the includesMap
				// and add it to the mergedIncludes slice at the correct position
				if _, ok := includesMap[key(include.Project, include.Ref)][file]; !ok {
					includesMap[key(include.Project, include.Ref)][file] = nil // track that the file is included

					mergedIncludes[pos].Files = append(mergedIncludes[pos].Files, file)
				}
			}
		}

		cfg.Includes = mergedIncludes
	}

	return cfg
}

func key(project string, ref *string) string {
	strRef := ""

	if ref != nil {
		strRef = *ref
	}

	return fmt.Sprintf("%s:%s", project, strRef)
}
