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
	"github.com/slack-go/slack"
)

const memberEntitlement = "member"

type workspaceResourceType struct {
	resourceType       *v2.ResourceType
	client             *slack.Client
	businessPlusClient *client.Client
	// skipWorkspaceRoleResourceType reports whether workspace_role is excluded
	// from the sync filter. Grants emits a mix: workspace_role assignments plus
	// the workspace's own member grants. Only the former are filtered, so this
	// gates workspaceRoleGrants rather than short-circuiting Grants.
	//
	// The SkipGrants / SkipEntitlementsAndGrants annotations are not usable
	// here for that same reason: they suppress the whole grants pass for the
	// resource, which would silently drop workspace membership.
	skipWorkspaceRoleResourceType bool
}

func (o *workspaceResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
}

func workspaceBuilder(
	slackClient *slack.Client,
	businessPlusClient *client.Client,
	skipWorkspaceRoleResourceType bool,
) *workspaceResourceType {
	return &workspaceResourceType{
		resourceType:                  resourceTypeWorkspace,
		skipWorkspaceRoleResourceType: skipWorkspaceRoleResourceType,
		client:                        slackClient,
		businessPlusClient:            businessPlusClient,
	}
}

// Create a new connector resource for a Slack workspace.
func workspaceResource(
	_ context.Context,
	workspace slack.Team,
	_ *v2.ResourceId,
) (*v2.Resource, error) {
	return resources.NewGroupResource(
		workspace.Name,
		resourceTypeWorkspace,
		workspace.ID,
		[]resources.GroupTraitOption{},
		resources.WithResourceProfile(
			map[string]interface{}{
				"workspace_id":     workspace.ID,
				"workspace_name":   workspace.Name,
				"workspace_domain": workspace.Domain,
			},
		),
		resources.WithAnnotation(
			&v2.ChildResourceType{ResourceTypeId: resourceTypeUser.Id},
			&v2.ChildResourceType{ResourceTypeId: resourceTypeUserGroup.Id},
			&v2.ChildResourceType{ResourceTypeId: resourceTypeWorkspaceRole.Id},
			&v2.ChildResourceType{ResourceTypeId: resourceTypeChannel.Id},
		),
	)
}

func (o *workspaceResourceType) List(
	ctx context.Context,
	parentID *v2.ResourceId,
	attrs resources.SyncOpAttrs,
) ([]*v2.Resource, *resources.SyncOpResults, error) {
	bag, err := pkg.ParsePageToken(attrs.PageToken.Token, &v2.ResourceId{ResourceType: resourceTypeWorkspace.Id})
	if err != nil {
		return nil, nil, fmt.Errorf("parsing page token: %w", err)
	}

	var (
		workspaces []slack.Team
		nextCursor string
	)
	var annos annotations.Annotations
	params := slack.ListTeamsParameters{Cursor: bag.PageToken()}
	workspaces, nextCursor, err = o.client.ListTeamsContext(ctx, params)
	if err != nil {
		return nil, &resources.SyncOpResults{Annotations: annos}, client.WrapError(err, "error listing teams", &annos)
	}

	err = client.SetWorkspaceNames(ctx, attrs.Session, workspaces)
	if err != nil {
		return nil, nil, fmt.Errorf("storing workspace names in session: %w", err)
	}

	rv := make([]*v2.Resource, 0, len(workspaces))
	for _, ws := range workspaces {
		resource, err := workspaceResource(ctx, ws, parentID)
		if err != nil {
			return nil, nil, fmt.Errorf("creating workspace resource: %w", err)
		}
		rv = append(rv, resource)
	}

	pageToken, err := bag.NextToken(nextCursor)
	if err != nil {
		return nil, nil, fmt.Errorf("creating next page token: %w", err)
	}
	return rv, &resources.SyncOpResults{
		NextPageToken: pageToken,
		Annotations:   annos,
	}, nil
}

func (o *workspaceResourceType) Entitlements(
	_ context.Context,
	resource *v2.Resource,
	attrs resources.SyncOpAttrs,
) ([]*v2.Entitlement, *resources.SyncOpResults, error) {
	return []*v2.Entitlement{
		entitlement.NewAssignmentEntitlement(
			resource,
			memberEntitlement,
			entitlement.WithGrantableTo(resourceTypeUser),
			entitlement.WithDescription(
				fmt.Sprintf(
					"Member of the %s workspace",
					resource.DisplayName,
				),
			),
			entitlement.WithDisplayName(
				fmt.Sprintf(
					"%s workspace member",
					resource.DisplayName,
				),
			),
		),
	}, &resources.SyncOpResults{}, nil
}

// sets workspace memberships and workspace roles.
func (o *workspaceResourceType) Grants(
	ctx context.Context,
	resource *v2.Resource,
	attrs resources.SyncOpAttrs,
) ([]*v2.Grant, *resources.SyncOpResults, error) {
	var (
		users             []client.User
		pageToken         string
		outputAnnotations annotations.Annotations
	)

	if o.businessPlusClient != nil {
		// Use business+ client with proper SDK pagination and team_id filtering.
		bag, err := pkg.ParsePageToken(attrs.PageToken.Token, &v2.ResourceId{ResourceType: resourceTypeUser.Id})
		if err != nil {
			return nil, nil, client.WrapError(err, "parsing page token", nil)
		}

		outputAnnotations = annotations.New()
		bpUsers, nextCursor, ratelimitData, err := o.businessPlusClient.GetUsers(
			ctx,
			resource.Id.Resource,
			bag.PageToken(),
		)
		outputAnnotations.WithRateLimiting(ratelimitData)
		if err != nil {
			return nil, &resources.SyncOpResults{Annotations: outputAnnotations}, client.WrapError(err, "fetching users for workspace", nil)
		}

		pt, err := bag.NextToken(nextCursor)
		if err != nil {
			return nil, nil, client.WrapError(err, "creating next page token", nil)
		}
		pageToken = pt
		users = bpUsers
	} else {
		// Fallback: standard Slack API with team_id filter and proper context.
		// The slack-go client auto-paginates internally.
		slackUsers, err := o.client.GetUsersContext(
			ctx,
			slack.GetUsersOptionTeamID(resource.Id.Resource),
		)
		if err != nil {
			return nil, &resources.SyncOpResults{Annotations: outputAnnotations}, client.WrapError(err, "fetching users for workspace", &outputAnnotations)
		}
		for _, u := range slackUsers {
			users = append(users, client.User{
				ID:                u.ID,
				Deleted:           u.Deleted,
				IsStranger:        u.IsStranger,
				IsPrimaryOwner:    u.IsPrimaryOwner,
				IsOwner:           u.IsOwner,
				IsAdmin:           u.IsAdmin,
				IsRestricted:      u.IsRestricted,
				IsUltraRestricted: u.IsUltraRestricted,
				IsInvitedUser:     u.IsInvitedUser,
				IsBot:             u.IsBot,
			})
		}
	}

	var rv []*v2.Grant
	for _, user := range users {
		if user.IsStranger {
			continue
		}
		userID, err := resources.NewResourceID(resourceTypeUser, user.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("creating user resource ID: %w", err)
		}

		// Only the workspace_role grants are conditional. The workspace's own
		// member grant below must still be emitted when workspace_role is
		// filtered out.
		if !o.skipWorkspaceRoleResourceType {
			roleGrants, err := workspaceRoleGrants(ctx, user, resource.Id, userID)
			if err != nil {
				return nil, nil, err
			}
			rv = append(rv, roleGrants...)
		}

		rv = append(rv, grant.NewGrant(resource, memberEntitlement, userID))
	}

	return rv, &resources.SyncOpResults{
		NextPageToken: pageToken,
		Annotations:   outputAnnotations,
	}, nil
}

// workspaceRoleGrants maps a workspace member's flags onto grants of the
// workspace_role resource type. Split out from Grants so that it can be skipped
// wholesale when workspace_role is excluded from the sync filter, without
// disturbing the workspace's own member grants.
func workspaceRoleGrants(
	ctx context.Context,
	user client.User,
	workspaceID *v2.ResourceId,
	userID *v2.ResourceId,
) ([]*v2.Grant, error) {
	var rv []*v2.Grant

	appendRole := func(roleID string, describe string) error {
		rr, err := roleResource(ctx, roleID, workspaceID)
		if err != nil {
			return fmt.Errorf("creating %s role resource: %w", describe, err)
		}
		rv = append(rv, grant.NewGrant(rr, RoleAssignmentEntitlement, userID))
		return nil
	}

	if user.IsPrimaryOwner {
		if err := appendRole(PrimaryOwnerRoleID, "primary owner"); err != nil {
			return nil, err
		}
	}

	if user.IsOwner {
		if err := appendRole(OwnerRoleID, "owner"); err != nil {
			return nil, err
		}
	}

	if user.IsAdmin {
		if err := appendRole(AdminRoleID, "admin"); err != nil {
			return nil, err
		}
	}

	if user.IsRestricted {
		if user.IsUltraRestricted {
			if err := appendRole(SingleChannelGuestRoleID, "single channel guest"); err != nil {
				return nil, err
			}
		} else {
			if err := appendRole(MultiChannelGuestRoleID, "multi channel guest"); err != nil {
				return nil, err
			}
		}
	}

	if user.IsInvitedUser {
		if err := appendRole(InvitedMemberRoleID, "invited member"); err != nil {
			return nil, err
		}
	}

	if !user.IsRestricted && !user.IsUltraRestricted && !user.IsInvitedUser && !user.IsBot && !user.Deleted {
		if err := appendRole(MemberRoleID, "member"); err != nil {
			return nil, err
		}
	}

	if user.IsBot {
		if err := appendRole(BotRoleID, "bot"); err != nil {
			return nil, err
		}
	}

	return rv, nil
}

// Grant and Revoke are not implemented for workspace membership because they require
// Enterprise Grid-only API endpoints (admin.users.assign and admin.users.remove).
// These endpoints are only available on Enterprise Grid plans, not Business+ plans.
