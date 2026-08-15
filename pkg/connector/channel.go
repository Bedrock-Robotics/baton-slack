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
		[]resources.GroupTraitOption{},
		resources.WithResourceProfile(profile),
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
		return nil, nil, fmt.Errorf("baton-slack: parsing page token: %w", err)
	}

	params := &slack.GetConversationsParameters{
		TeamID:          parentResourceID.Resource,
		Cursor:          bag.PageToken(),
		ExcludeArchived: true,
		Limit:           channelPageSize,
		Types: []string{"public_channel", "private_channel"},
	}

	var outputAnnotations annotations.Annotations
	channels, nextCursor, err := c.client.GetConversationsContext(ctx, params)
	if err != nil {
		return nil, &resources.SyncOpResults{Annotations: outputAnnotations}, client.WrapError(
			err,
			fmt.Sprintf("listing channels for team %s", parentResourceID.Resource),
			&outputAnnotations,
		)
	}

	rv := make([]*v2.Resource, 0, len(channels))
	for _, ch := range channels {
		resource, err := channelResource(ctx, ch, parentResourceID)
		if err != nil {
			return nil, nil, fmt.Errorf("baton-slack: creating channel resource: %w", err)
		}
		rv = append(rv, resource)
	}

	pageToken, err := bag.NextToken(nextCursor)
	if err != nil {
		return nil, nil, fmt.Errorf("baton-slack: creating next page token: %w", err)
	}

	return rv, &resources.SyncOpResults{NextPageToken: pageToken, Annotations: outputAnnotations}, nil
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
		return nil, nil, fmt.Errorf("baton-slack: parsing page token: %w", err)
	}

	params := &slack.GetUsersInConversationParameters{
		ChannelID: resource.Id.Resource,
		Cursor:    bag.PageToken(),
		Limit:     channelPageSize,
	}

	var outputAnnotations annotations.Annotations
	members, nextCursor, err := c.client.GetUsersInConversationContext(ctx, params)
	if err != nil {
		return nil, &resources.SyncOpResults{Annotations: outputAnnotations}, client.WrapError(
			err,
			fmt.Sprintf("fetching channel members for channel %s", resource.Id.Resource),
			&outputAnnotations,
		)
	}

	var rv []*v2.Grant
	for _, memberID := range members {
		userID, err := resources.NewResourceID(resourceTypeUser, memberID)
		if err != nil {
			return nil, nil, fmt.Errorf("baton-slack: creating user resource ID: %w", err)
		}
		rv = append(rv, grant.NewGrant(resource, memberEntitlement, userID))
	}

	pageToken, err := bag.NextToken(nextCursor)
	if err != nil {
		return nil, nil, fmt.Errorf("baton-slack: creating next page token: %w", err)
	}

	return rv, &resources.SyncOpResults{NextPageToken: pageToken, Annotations: outputAnnotations}, nil
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
		return nil, fmt.Errorf("baton-slack: only users can be granted channel membership")
	}

	channelID := ent.Resource.Id.Resource
	userID := principal.Id.Resource

	var outputAnnotations annotations.Annotations
	_, err := c.client.InviteUsersToConversationContext(ctx, channelID, userID)
	if err != nil {
		switch client.SlackErrorCode(err) {
		case client.SlackErrAlreadyInChannel:
			outputAnnotations.Append(&v2.GrantAlreadyExists{})
			return outputAnnotations, nil
		case client.SlackErrCantInviteSelf:
			return outputAnnotations, fmt.Errorf(
				"baton-slack: Slack refuses to invite the connector's own bot user to channel %s: %w", channelID, err)
		case client.SlackErrChannelNotFound:
			return outputAnnotations, fmt.Errorf(
				"baton-slack: channel %s does not exist, or the connector's bot user is not a member of it: %w", channelID, err)
		case client.SlackErrIsArchived:
			return outputAnnotations, fmt.Errorf(
				"baton-slack: channel %s is archived; unarchive it in Slack before granting membership: %w", channelID, err)
		}
		return outputAnnotations, client.WrapError(err, "baton-slack: inviting user to channel", &outputAnnotations)
	}

	return outputAnnotations, nil
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
		return nil, fmt.Errorf("baton-slack: only users can have channel membership revoked")
	}

	channelID := ent.Resource.Id.Resource
	userID := principal.Id.Resource

	var outputAnnotations annotations.Annotations
	err := c.client.KickUserFromConversationContext(ctx, channelID, userID)
	if err != nil {
		switch client.SlackErrorCode(err) {
		case client.SlackErrNotInChannel:
			outputAnnotations.Append(&v2.GrantAlreadyRevoked{})
			return outputAnnotations, nil
		case client.SlackErrCantKickSelf:
			return outputAnnotations, fmt.Errorf(
				"baton-slack: Slack refuses to remove the connector's own bot user from channel %s: %w", channelID, err)
		case client.SlackErrCantKickFromGeneral:
			return outputAnnotations, fmt.Errorf(
				"baton-slack: Slack does not allow removing a user from the workspace default channel (%s); this is permanent, a retry cannot succeed: %w",
				channelID, err)
		case client.SlackErrRestrictedAction:
			return outputAnnotations, fmt.Errorf(
				"baton-slack: the Slack workspace forbids this app from removing members of channel %s; a workspace admin must change the preference, a retry cannot succeed: %w",
				channelID, err)
		}
		return outputAnnotations, client.WrapError(err, "baton-slack: removing user from channel", &outputAnnotations)
	}

	return outputAnnotations, nil
}
