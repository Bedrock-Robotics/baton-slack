package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/slack-go/slack"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// defaultRateLimitRetryAfter is used when Slack returns a rate limit error
// without a Retry-After header (e.g. ok:false with "ratelimited" on HTTP 200).
const defaultRateLimitRetryAfter = 30 * time.Second

func logBody(ctx context.Context, response *http.Response) {
	l := ctxzap.Extract(ctx)

	if response == nil {
		l.Error("response is nil")
		return
	}

	if response.Body == nil {
		l.Error("response body is nil")
		return
	}

	body := make([]byte, 512)
	_, err := response.Body.Read(body)
	if err != nil {
		l.Error("error reading response body", zap.Error(err))
		return
	}
	l.Info("response body", zap.String("body", string(body)))
}

// Inspects the error returned by the slack-go library and maps it to appropriate gRPC codes.
// If annos is non-nil, rate limit information will be appended to it so the caller can
// include it in SyncOpResults.
func WrapError(err error, contextMsg string, annos *annotations.Annotations) error {
	if err == nil {
		return nil
	}

	// for rate limit errors
	var slackLibrateLimitErr *slack.RateLimitedError
	if errors.As(err, &slackLibrateLimitErr) {
		rlDesc := rateLimitDescription(slackLibrateLimitErr.RetryAfter)
		if annos != nil {
			annos.WithRateLimiting(rlDesc)
		}
		return wrapErrorWithRateLimitDetails(codes.Unavailable, contextMsg, rlDesc, err)
	}

	// for 5xx status codes
	var slackLibErr slack.StatusCodeError
	if errors.As(err, &slackLibErr) {
		grpcCode := httpStatusToGRPCCode(slackLibErr.Code)
		contextMsg = fmt.Sprintf("Slack-go API HTTP error: %s : %s", slackLibErr.Status, contextMsg)
		return uhttp.WrapErrors(grpcCode, contextMsg, err)
	}

	// when ok: false even with 200 HTTP status codes
	var slackErrResp slack.SlackErrorResponse
	if errors.As(err, &slackErrResp) {
		grpcCode := MapSlackErrorToGRPCCode(slackErrResp.Err)
		if len(slackErrResp.ResponseMetadata.Messages) > 0 {
			contextMsg = fmt.Sprintf("%s (details: %v)", contextMsg, slackErrResp.ResponseMetadata.Messages)
		}
		if len(slackErrResp.ResponseMetadata.Warnings) > 0 {
			contextMsg = fmt.Sprintf("%s (warnings: %v)", contextMsg, slackErrResp.ResponseMetadata.Warnings)
		}
		// Slack can return ok:false with "ratelimited" on HTTP 200. There's no
		// Retry-After header in this case, so use a default backoff.
		if grpcCode == codes.Unavailable {
			rlDesc := rateLimitDescription(defaultRateLimitRetryAfter)
			if annos != nil {
				annos.WithRateLimiting(rlDesc)
			}
			return wrapErrorWithRateLimitDetails(grpcCode, contextMsg, rlDesc, err)
		}
		return uhttp.WrapErrors(grpcCode, contextMsg, err)
	}

	// Preserve the gRPC code if the error already has one (e.g. from uhttp/businessPlusClient).
	// Only fall back to Unknown if no code is set.
	if code := status.Code(err); code != codes.OK {
		return uhttp.WrapErrors(code, contextMsg, err)
	}
	return uhttp.WrapErrors(codes.Unknown, contextMsg, err)
}

// SlackErrorCode returns the Slack API error string carried by err, for example
// "already_in_channel". It returns "" when err is not a Slack API error.
func SlackErrorCode(err error) string {
	var slackErrResp slack.SlackErrorResponse
	if errors.As(err, &slackErrResp) {
		return slackErrResp.Err
	}
	return ""
}

func httpStatusToGRPCCode(httpStatus int) codes.Code {
	switch httpStatus {
	case http.StatusBadRequest:
		return codes.Internal
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	case http.StatusForbidden:
		return codes.PermissionDenied
	case http.StatusNotFound:
		return codes.NotFound
	case http.StatusConflict:
		return codes.AlreadyExists
	case http.StatusTooManyRequests:
		return codes.Unavailable
	case http.StatusInternalServerError:
		return codes.Internal
	case http.StatusServiceUnavailable:
		return codes.Unavailable
	default:
		return codes.Unknown
	}
}

func containsAny(s string, substrs ...string) bool {
	for _, substr := range substrs {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// maps Slack error strings to gRPC codes.
func MapSlackErrorToGRPCCode(slackError string) codes.Code {
	err := strings.ToLower(slackError)

	if containsAny(err, "invalid_auth", "token_revoked", "token_expired", "not_authed", "account_inactive") {
		return codes.Unauthenticated
	}

	if containsAny(err, "missing_scope", "access_denied", "not_allowed_token_type",
		"team_access_not_granted", "no_permission", "ekm_access_denied") {
		return codes.PermissionDenied
	}

	if containsAny(err, "ratelimited") {
		return codes.Unavailable
	}

	if containsAny(err, "user_not_found", "user_already_deleted") {
		return codes.NotFound
	}

	// no_such_subteam is returned when fetching user group members. return NotFound to indicate the group is disabled or deleted.
	if containsAny(err, SlackErrNoSuchSubteam) {
		return codes.NotFound
	}

	if containsAny(err, "user_already_team_member") {
		return codes.AlreadyExists
	}

	if containsAny(err, "invalid_arguments", "missing_argument", "invalid_arg_name",
		"invalid_array_arg", "invalid_charset", "invalid_form_data", "invalid_post_type",
		"missing_post_type", "limit_required") {
		return codes.InvalidArgument
	}

	if containsAny(err, "two_factor_setup_required") {
		return codes.FailedPrecondition
	}

	if containsAny(err, "internal_error", "service_unavailable", "request_timeout") {
		return codes.Unavailable
	}

	if containsAny(err, "fatal_error") {
		return codes.Internal
	}

	if containsAny(err, "method_deprecated", "deprecated_endpoint") {
		return codes.Unimplemented
	}

	return codes.Unknown
}

// IsRateLimited checks whether the annotations contain a RateLimitDescription
// with STATUS_OVERLIMIT.
func IsRateLimited(annos *annotations.Annotations) bool {
	if annos == nil {
		return false
	}
	rl := &v2.RateLimitDescription{}
	ok, err := annos.Pick(rl)
	if err != nil || !ok {
		return false
	}
	return rl.Status == v2.RateLimitDescription_STATUS_OVERLIMIT
}

// RateLimitOverride returns a RateLimitDescription with a 60s wait.
// Use this for endpoints where Slack's Retry-After header is too short
// to allow meaningful progress (e.g. users.info in a per-member loop).
func RateLimitOverride() *v2.RateLimitDescription {
	return rateLimitDescription(60 * time.Second)
}

// WrapErrorWithRateLimitOverride re-wraps an error with a 60s rate limit on
// both the gRPC status details (for SDK retry) and the annotations (for SyncOpResults).
func WrapErrorWithRateLimitOverride(err error, annos *annotations.Annotations) error {
	rlDesc := RateLimitOverride()
	if annos != nil {
		annos.WithRateLimiting(rlDesc)
	}
	return wrapErrorWithRateLimitDetails(codes.Unavailable, "rate limited", rlDesc, err)
}

// wrapErrorWithRateLimitDetails creates a gRPC error with RateLimitDescription
// attached as a status detail, so the SDK's retry logic knows how long to wait.
func wrapErrorWithRateLimitDetails(code codes.Code, msg string, rlDesc *v2.RateLimitDescription, err error) error {
	st := status.New(code, msg)
	if rlDesc != nil {
		st, _ = st.WithDetails(rlDesc)
	}
	return errors.Join(st.Err(), err)
}

func rateLimitDescription(retryAfter time.Duration) *v2.RateLimitDescription {
	return &v2.RateLimitDescription{
		Status:    v2.RateLimitDescription_STATUS_OVERLIMIT,
		Remaining: 0,
		ResetAt:   timestamppb.New(time.Now().Add(retryAfter)),
	}
}

// Slack API may return errors in the response body even when the HTTP status code is 200.
//
//	examples:
//
//	{"ok":false,"error":"invalid_auth"}
//	{"ok":false,"error": "user_not_found"}
func ErrorWithGrpcCodeFromBytes(bodyBytes []byte) error {
	var res SlackErrorResponse
	if err := json.Unmarshal(bodyBytes, &res); err != nil {
		return fmt.Errorf("error parsing Slack API response: %w", err)
	}

	if !res.Ok && res.Error != "" {
		grpcCode := MapSlackErrorToGRPCCode(res.Error)
		return uhttp.WrapErrors(
			grpcCode,
			fmt.Sprintf("Slack API error: %s", res.Error),
			fmt.Errorf("slack error: %s", res.Error),
		)
	}

	return nil
}
