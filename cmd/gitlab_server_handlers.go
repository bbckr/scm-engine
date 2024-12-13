package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jippi/scm-engine/pkg/config"
	"github.com/jippi/scm-engine/pkg/state"
	slogctx "github.com/veqryn/slog-context"
)

func GitLabStatusHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	slogctx.Debug(ctx, "GET /_status")

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("scm-engine status: OK\n\nNOTE: this is a static 'OK', no actual checks are being made"))
}

func GitLabWebhookHandler(ctx context.Context, webhookSecret string) http.HandlerFunc {
	// Initialize GitLab client
	client, err := getClient(ctx)
	if err != nil {
		panic(err)
	}

	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// Check if the webhook secret is set (and if its matching)
		if len(webhookSecret) > 0 {
			theirSecret := r.Header.Get("X-Gitlab-Token")
			if webhookSecret != theirSecret {
				errHandler(ctx, w, http.StatusForbidden, errors.New("Missing or invalid X-Gitlab-Token header"))

				return
			}
		}

		// Validate content type
		if r.Header.Get("Content-Type") != "application/json" {
			errHandler(ctx, w, http.StatusNotAcceptable, errors.New("The request is not using Content-Type: application/json"))

			return
		}

		// Read the POST body of the request
		body, err := io.ReadAll(r.Body)
		if err != nil {
			errHandler(ctx, w, http.StatusBadRequest, err)

			return
		}

		// Ensure we have content in the POST body
		if len(body) == 0 {
			errHandler(ctx, w, http.StatusBadRequest, errors.New("The POST body is empty; expected a JSON payload"))
		}

		// Decode request payload
		var payload GitlabWebhookPayload
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(&payload); err != nil {
			errHandler(ctx, w, http.StatusBadRequest, fmt.Errorf("could not decode POST body into Payload struct: %w", err))

			return
		}

		// Initialize context
		ctx = state.WithProjectID(ctx, payload.Project.PathWithNamespace)

		// Grab event specific information
		var (
			id     string
			gitSha string
		)

		switch payload.EventType {
		case "merge_request":
			id = strconv.Itoa(payload.ObjectAttributes.IID)
			gitSha = payload.ObjectAttributes.LastCommit.ID

		case "note":
			id = strconv.Itoa(payload.MergeRequest.IID)
			gitSha = payload.MergeRequest.LastCommit.ID

		default:
			errHandler(ctx, w, http.StatusInternalServerError, fmt.Errorf("unknown event type: %s", payload.EventType))

			return
		}

		// Build context for rest of the pipeline
		ctx = state.WithCommitSHA(ctx, gitSha)
		ctx = state.WithMergeRequestID(ctx, id)
		ctx = slogctx.With(ctx, slog.String("event_type", payload.EventType))

		slogctx.Info(ctx, "GET /gitlab webhook")

		// Decode request payload into 'any' so we have all the details
		var fullEventPayload any
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(&fullEventPayload); err != nil {
			errHandler(ctx, w, http.StatusInternalServerError, err)

			return
		}

		// Check if there exists scm-config file in the repo before moving forward
		file, err := client.MergeRequests().GetRemoteConfig(ctx, state.ConfigFilePath(ctx), state.CommitSHA(ctx))
		// only error when global config is not set
		if err != nil && state.GlobalConfigFilePath(ctx) == "" {
			errHandler(ctx, w, http.StatusOK, err)

			return
		}

		// Try to parse the config file
		//
		// In case of a parse error cfg remains "nil" and ProcessMR will try to read-and-parse it
		// (but obviously also fail), but will surface the error within the GitLab External Pipeline (if enabled)
		// which will surface the issue to the end-user directly
		var cfg *config.Config
		if file != nil { // file could be nil if no scm-config file is found when global config is set
			cfg, _ = config.ParseFile(file)
		} else {
			// avoid trying to read-and-parse again if global config is set
			cfg = config.GlobalConfigFromContext(ctx)
		}

		// Process the MR
		if err := ProcessMR(ctx, client, cfg, fullEventPayload); err != nil {
			errHandler(ctx, w, http.StatusOK, err)

			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}
}
