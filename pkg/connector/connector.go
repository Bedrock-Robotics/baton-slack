package connector

import (
	"context"
	"fmt"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/cli"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	cfg "github.com/conductorone/baton-slack/pkg/config"
	"github.com/conductorone/baton-slack/pkg/connector/client"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/slack-go/slack"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
)

type Slack struct {
	client             *slack.Client
	apiKey             string
	businessPlusClient *client.Client
	govEnv             bool
	// skipWorkspaceRoleResourceType reports whether workspace_role is excluded
	// from the sync filter. Named for the skip condition so the zero value is
	// the safe default.
	skipWorkspaceRoleResourceType bool
}

const govSlackApiUrl = "https://api.slack-gov.com/api/"

// Metadata returns metadata about the connector.
func (c *Slack) Metadata(ctx context.Context) (*v2.ConnectorMetadata, error) {
	return &v2.ConnectorMetadata{
		DisplayName: "Slack",
		Description: "Connector syncing users, workspaces, user groups, workspace roles and channels from Slack to Baton.",
		AccountCreationSchema: &v2.ConnectorAccountCreationSchema{
			FieldMap: map[string]*v2.ConnectorAccountCreationSchema_Field{
				"channel_ids": {
					DisplayName: "ChannelIDs",
					Required:    true,
					Description: "Channel IDs the user will be invited to",
					Field: &v2.ConnectorAccountCreationSchema_Field_StringField{
						StringField: &v2.ConnectorAccountCreationSchema_StringField{},
					},
					Placeholder: "ChannelIDs",
					Order:       1,
				},
				"email": {
					DisplayName: "Email",
					Required:    true,
					Description: "This email will be used as the login for the user.",
					Field: &v2.ConnectorAccountCreationSchema_Field_StringField{
						StringField: &v2.ConnectorAccountCreationSchema_StringField{},
					},
					Placeholder: "Email",
					Order:       2,
				},
				"team_id": {
					DisplayName: "WorkspaceID",
					Required:    true,
					Description: "The workspaceID the user will be invited to",
					Field: &v2.ConnectorAccountCreationSchema_Field_StringField{
						StringField: &v2.ConnectorAccountCreationSchema_StringField{},
					},
					Placeholder: "TeamID",
					Order:       3,
				},
			},
		},
	}, nil
}

// Validate hits the Slack API to validate that the authenticated user has needed permissions.
func (s *Slack) Validate(ctx context.Context) (annotations.Annotations, error) {
	res, err := s.client.AuthTestContext(ctx)
	if err != nil {
		return nil, client.WrapError(err, "authenticating", nil)
	}

	user, err := s.client.GetUserInfoContext(ctx, res.UserID)
	if err != nil {
		return nil, client.WrapError(err, "retrieving authenticated user", nil)
	}

	isValidUser := user.IsAdmin || user.IsOwner || user.IsPrimaryOwner || user.IsBot
	if !isValidUser {
		return nil, uhttp.WrapErrors(
			codes.PermissionDenied,
			"authenticated user is not an admin, owner, primary owner or a bot",
			fmt.Errorf("user lacks required permissions"),
		)
	}
	return nil, nil
}

type slackLogger struct {
	ZapLog *zap.Logger
}

// Output Needed to prevent slack client from logging in its own format.
func (s *slackLogger) Output(callDepth int, msg string) error {
	s.ZapLog.Info(msg, zap.Int("callDepth", callDepth))
	return nil
}

func NewSlack(ctx context.Context, apiKey, businessPlusKey string, govEnv bool, baseURL string) (*Slack, error) {
	l := ctxzap.Extract(ctx)
	httpClient, err := uhttp.NewClient(ctx, uhttp.WithLogger(true, l))
	if err != nil {
		return nil, err
	}

	logger := &slackLogger{ZapLog: l}
	opts := []slack.Option{
		slack.OptionDebug(true),
		slack.OptionHTTPClient(httpClient),
		slack.OptionLog(logger),
	}
	// Custom base URL takes precedence over gov environment
	if baseURL != "" {
		opts = append(opts, slack.OptionAPIURL(baseURL))
	} else if govEnv {
		opts = append(opts, slack.OptionAPIURL(govSlackApiUrl))
	}
	slackClient := slack.New(apiKey, opts...)

	var businessPlusClient *client.Client
	if businessPlusKey != "" {
		var err error
		businessPlusClient, err = client.NewClient(
			httpClient,
			businessPlusKey,
			apiKey,
			govEnv,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create Business+ client. Error: %w", err)
		}
	}

	return &Slack{
		client:             slackClient,
		apiKey:             apiKey,
		businessPlusClient: businessPlusClient,
		govEnv:             govEnv,
	}, nil
}

func New(ctx context.Context, config *cfg.Slack, opts *cli.ConnectorOpts) (connectorbuilder.ConnectorBuilderV2, []connectorbuilder.Opt, error) {
	cb, err := NewSlack(
		ctx,
		config.Token,
		config.BusinessPlusToken,
		config.GovEnv,
		config.BaseUrl,
	)
	if err != nil {
		return nil, nil, err
	}

	// nil opts means no filter, so nothing is skipped.
	cb.skipWorkspaceRoleResourceType = opts != nil && !opts.WillSyncResourceType(resourceTypeWorkspaceRole.Id)

	builderOpts := []connectorbuilder.Opt{}
	return cb, builderOpts, nil
}

func (s *Slack) ResourceSyncers(ctx context.Context) []connectorbuilder.ResourceSyncerV2 {
	return []connectorbuilder.ResourceSyncerV2{
		userBuilder(s.client, s.businessPlusClient),
		workspaceBuilder(s.client, s.businessPlusClient, s.skipWorkspaceRoleResourceType),
		userGroupBuilder(s.client, s.businessPlusClient),
		groupBuilder(s.businessPlusClient, s.govEnv),
		workspaceRoleBuilder(s.businessPlusClient),
		channelBuilder(s.client),
	}
}
