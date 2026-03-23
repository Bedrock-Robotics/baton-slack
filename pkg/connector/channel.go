package connector

import (
	"context"
	"fmt"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	resources "github.com/conductorone/baton-sdk/pkg/types/resource"

	"github.com/conductorone/baton-slack/pkg"
	"github.com/conductorone/baton-slack/pkg/connector/client"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

const channelPageSize = 200

type channelResourceType struct {
	resourceType *v2.ResourceType
	client       *slack.Client
}

func (c *channelResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return c.resourceType
}

func channelBuilder(slackClient *slack.Client) *channelResourceType {
	return &channelResourceType{
		resourceType: resourceTypeChannel,
		client:       slackClient,
	}
}

// channelResource creates a new connector resource for a Slack channel.
func channelResource(
	_ context.Context,
	channel slack.Channel,
	parentResourceID *v2.ResourceId,
) (*v2.Resource, error) {
	profile := map[string]interface{}{
		"channel_id":   channel.ID,
		"channel_name": channel.Name,
	}
	if channel.Topic.Value != "" {
		profile["channel_topic"] = channel.Topic.Value
	}
	if channel.Purpose.Value != "" {
		profile["channel_purpose"] = channel.Purpose.Value
	}

	return resources.NewGroupResource(
		channel.Name,
		resourceTypeChannel,
		channel.ID,
		[]resources.GroupTraitOption{
			resources.WithGroupProfile(profile),
		},
		resources.WithParentResourceID(parentResourceID),
	)
}

func (c *channelResourceType) List(
	ctx context.Context,
	parentResourceID *v2.ResourceId,
	attrs resources.SyncOpAttrs,
) ([]*v2.Resource, *resources.SyncOpResults, error) {
	if parentResourceID == nil {
		return nil, &resources.SyncOpResults{}, nil
	}

	bag, err := pkg.ParsePageToken(attrs.PageToken.Token, &v2.ResourceId{ResourceType: resourceTypeChannel.Id})
	if err != nil {
		return nil, nil, fmt.Errorf("parsing page token: %w", err)
	}

	params := &slack.GetConversationsParameters{
		TeamID:          parentResourceID.Resource,
		Cursor:          bag.PageToken(),
		ExcludeArchived: true,
		Limit:           channelPageSize,
		Types:           []string{"public_channel", "private_channel"},
	}

	channels, nextCursor, err := c.client.GetConversationsContext(ctx, params)
	if err != nil {
		return nil, nil, client.WrapError(err, fmt.Sprintf("listing channels for team %s", parentResourceID.Resource))
	}

	rv := make([]*v2.Resource, 0, len(channels))
	for _, ch := range channels {
		resource, err := channelResource(ctx, ch, parentResourceID)
		if err != nil {
			return nil, nil, fmt.Errorf("creating channel resource: %w", err)
		}
		rv = append(rv, resource)
	}

	pageToken, err := bag.NextToken(nextCursor)
	if err != nil {
		return nil, nil, fmt.Errorf("creating next page token: %w", err)
	}

	return rv, &resources.SyncOpResults{NextPageToken: pageToken}, nil
}

func (c *channelResourceType) Entitlements(
	_ context.Context,
	resource *v2.Resource,
	_ resources.SyncOpAttrs,
) ([]*v2.Entitlement, *resources.SyncOpResults, error) {
	return []*v2.Entitlement{
		entitlement.NewAssignmentEntitlement(
			resource,
			memberEntitlement,
			entitlement.WithGrantableTo(resourceTypeUser),
			entitlement.WithDescription(
				fmt.Sprintf(
					"Member of %s channel",
					resource.DisplayName,
				),
			),
			entitlement.WithDisplayName(
				fmt.Sprintf(
					"%s channel %s",
					resource.DisplayName,
					memberEntitlement,
				),
			),
		),
	}, &resources.SyncOpResults{}, nil
}

func (c *channelResourceType) Grants(
	ctx context.Context,
	resource *v2.Resource,
	attrs resources.SyncOpAttrs,
) ([]*v2.Grant, *resources.SyncOpResults, error) {
	bag, err := pkg.ParsePageToken(attrs.PageToken.Token, &v2.ResourceId{ResourceType: resourceTypeUser.Id})
	if err != nil {
		return nil, nil, fmt.Errorf("parsing page token: %w", err)
	}

	params := &slack.GetUsersInConversationParameters{
		ChannelID: resource.Id.Resource,
		Cursor:    bag.PageToken(),
		Limit:     channelPageSize,
	}

	members, nextCursor, err := c.client.GetUsersInConversationContext(ctx, params)
	if err != nil {
		return nil, nil, client.WrapError(err, fmt.Sprintf("fetching channel members for channel %s", resource.Id.Resource))
	}

	var rv []*v2.Grant
	for _, memberID := range members {
		userID, err := resources.NewResourceID(resourceTypeUser, memberID)
		if err != nil {
			return nil, nil, fmt.Errorf("creating user resource ID: %w", err)
		}
		rv = append(rv, grant.NewGrant(resource, memberEntitlement, userID))
	}

	pageToken, err := bag.NextToken(nextCursor)
	if err != nil {
		return nil, nil, fmt.Errorf("creating next page token: %w", err)
	}

	return rv, &resources.SyncOpResults{NextPageToken: pageToken}, nil
}

func (c *channelResourceType) Grant(
	ctx context.Context,
	principal *v2.Resource,
	ent *v2.Entitlement,
) (annotations.Annotations, error) {
	logger := ctxzap.Extract(ctx)

	if principal.Id.ResourceType != resourceTypeUser.Id {
		logger.Warn(
			"baton-slack: only users can be added to a channel",
			zap.String("principal_type", principal.Id.ResourceType),
			zap.String("principal_id", principal.Id.Resource),
		)
		return nil, fmt.Errorf("only users can be granted channel membership")
	}

	channelID := ent.Resource.Id.Resource
	userID := principal.Id.Resource

	_, err := c.client.InviteUsersToConversationContext(ctx, channelID, userID)
	if err != nil {
		// already_in_channel means the user is already a member - treat as success.
		if slackErr, ok := err.(slack.SlackErrorResponse); ok {
			if slackErr.Err == "already_in_channel" {
				return nil, nil
			}
		}
		return nil, fmt.Errorf("inviting user to channel: %w", err)
	}

	return nil, nil
}

func (c *channelResourceType) Revoke(
	ctx context.Context,
	grantToRevoke *v2.Grant,
) (annotations.Annotations, error) {
	logger := ctxzap.Extract(ctx)

	principal := grantToRevoke.Principal
	ent := grantToRevoke.Entitlement

	if principal.Id.ResourceType != resourceTypeUser.Id {
		logger.Warn(
			"baton-slack: only users can be removed from a channel",
			zap.String("principal_type", principal.Id.ResourceType),
			zap.String("principal_id", principal.Id.Resource),
		)
		return nil, fmt.Errorf("only users can have channel membership revoked")
	}

	channelID := ent.Resource.Id.Resource
	userID := principal.Id.Resource

	err := c.client.KickUserFromConversationContext(ctx, channelID, userID)
	if err != nil {
		// not_in_channel means the user is already not a member - treat as already revoked.
		if slackErr, ok := err.(slack.SlackErrorResponse); ok {
			if slackErr.Err == "not_in_channel" {
				outputAnnotations := annotations.New()
				outputAnnotations.Append(&v2.GrantAlreadyRevoked{})
				return outputAnnotations, nil
			}
		}
		return nil, fmt.Errorf("removing user from channel: %w", err)
	}

	return nil, nil
}
