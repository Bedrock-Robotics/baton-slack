package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/session"
	"github.com/conductorone/baton-sdk/pkg/types/sessions"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/slack-go/slack"
)

const (
	PageSizeDefault = 100

	// Slack API error string constants.
	SlackErrUserAlreadyTeamMember = "user_already_team_member"
	SlackErrUserAlreadyDeleted    = "user_already_deleted"
	// SlackErrNoSuchSubteam is returned when fetching user group members. Retrying
	// on this error was determined empirically to allow syncs to complete.
	SlackErrNoSuchSubteam = "no_such_subteam"

	// conversations.invite and conversations.kick error strings.
	SlackErrAlreadyInChannel = "already_in_channel"
	SlackErrNotInChannel     = "not_in_channel"
	SlackErrCantInviteSelf   = "cant_invite_self"
	SlackErrChannelNotFound  = "channel_not_found"
	SlackErrIsArchived       = "is_archived"
	SlackErrCantKickSelf     = "cant_kick_self"
	// SlackErrCantKickFromGeneral and SlackErrRestrictedAction are permanent.
	// The first is a Slack product rule, the second a workspace preference that
	// only an admin can change. Neither clears on retry.
	SlackErrCantKickFromGeneral = "cant_kick_from_general"
	SlackErrRestrictedAction    = "restricted_action"

	ScimVersionV2 = "v2"
	ScimVersionV1 = "v1"

	teamIDKey      = "team_id"
	scimPatchOpURN = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
)

var workspaceNameNamespace = sessions.WithPrefix("workspace_name")

type Client struct {
	baseScimUrl *url.URL
	baseUrl     *url.URL
	token       string
	botToken    string
	scimVersion string
	wrapper     *uhttp.BaseHttpClient
}

func NewClient(
	httpClient *http.Client,
	token string,
	botToken string,
	govEnv bool,
) (*Client, error) {
	finalBaseUrl := baseUrl
	finalBaseScimUrl := baseScimUrl
	finalScimVersion := ScimVersionV2
	if govEnv {
		finalBaseUrl = baseGovUrl
		finalBaseScimUrl = baseGovScimUrl
		finalScimVersion = ScimVersionV1
	}

	baseUrl0, err := url.Parse(finalBaseUrl)
	if err != nil {
		return nil, err
	}

	baseScimUrl0, err := url.Parse(finalBaseScimUrl)
	if err != nil {
		return nil, err
	}

	return &Client{
		baseUrl:     baseUrl0,
		baseScimUrl: baseScimUrl0,
		token:       token,
		botToken:    botToken,
		scimVersion: finalScimVersion,
		wrapper:     uhttp.NewBaseHttpClient(httpClient),
	}, nil
}

// GetUserInfo returns the user info for the given user ID.
func (c *Client) GetUserInfo(
	ctx context.Context,
	userID string,
) (
	*User,
	*v2.RateLimitDescription,
	error,
) {
	var response struct {
		BaseResponse
		User *User `json:"user"`
	}

	ratelimitData, err := c.post(
		ctx,
		UrlPathGetUserInfo,
		&response,
		map[string]interface{}{"user": userID},
		true,
	)
	if err != nil {
		return nil, ratelimitData, err
	}

	return response.User, ratelimitData, nil
}

// GetUserGroupMembers returns the members of the given user group from a given team.
func (c *Client) GetUserGroupMembers(
	ctx context.Context,
	userGroupID string,
	teamID string,
) (
	[]string,
	*v2.RateLimitDescription,
	error,
) {
	var response struct {
		BaseResponse
		Users []string `json:"users"`
	}

	ratelimitData, err := c.post(
		ctx,
		UrlPathGetUserGroupMembers,
		&response,
		map[string]interface{}{
			teamIDKey:   teamID,
			"usergroup": userGroupID,
		},
		true,
	)
	if err != nil {
		return nil, ratelimitData, err
	}

	return response.Users, ratelimitData, nil
}

// GetUsers returns the users of the given team.
func (c *Client) GetUsers(
	ctx context.Context,
	teamID string,
	cursor string,
) (
	[]User,
	string,
	*v2.RateLimitDescription,
	error,
) {
	values := map[string]interface{}{teamIDKey: teamID}

	// need to check if cursor is empty because API throws error if empty string is passed
	if cursor != "" {
		values["cursor"] = cursor
	}

	var response struct {
		BaseResponse
		Users []User `json:"members"`
		Pagination
	}

	ratelimitData, err := c.post(
		ctx,
		UrlPathGetUsers,
		&response,
		values,
		true,
	)
	if err != nil {
		return nil, "", ratelimitData, err
	}

	return response.Users,
		response.ResponseMetadata.NextCursor,
		ratelimitData,
		nil
}

// GetUserGroups returns the user groups for the given team.
func (c *Client) GetUserGroups(
	ctx context.Context,
	teamID string,
) (
	[]slack.UserGroup,
	*v2.RateLimitDescription,
	error,
) {
	var response struct {
		BaseResponse
		UserGroups []slack.UserGroup `json:"usergroups"`
	}

	ratelimitData, err := c.post(
		ctx,
		UrlPathGetUserGroups,
		&response,
		map[string]interface{}{teamIDKey: teamID},
		true,
	)
	if err != nil {
		return nil, ratelimitData, err
	}

	return response.UserGroups, ratelimitData, nil
}

// GetAuthTeamsList returns the list of teams for which the app is authed.
func (c *Client) GetAuthTeamsList(
	ctx context.Context,
	cursor string,
) (
	[]slack.Team,
	string,
	*v2.RateLimitDescription,
	error,
) {
	values := map[string]interface{}{}

	if cursor != "" {
		values["cursor"] = cursor
	}

	var response struct {
		BaseResponse
		Teams []slack.Team `json:"teams"`
		Pagination
	}

	ratelimitData, err := c.post(
		ctx,
		UrlPathAuthTeamsList,
		&response,
		values,
		false,
	)
	if err != nil {
		return nil, "", ratelimitData, err
	}

	return response.Teams,
		response.ResponseMetadata.NextCursor,
		ratelimitData,
		nil
}

// ListIDPGroups returns all IDP groups from the SCIM API.
func (c *Client) ListIDPGroups(
	ctx context.Context,
	startIndex int,
	count int,
) (
	*SCIMResponse[GroupResource],
	*v2.RateLimitDescription,
	error,
) {
	var response SCIMResponse[GroupResource]
	urlPathIDPGroups := fmt.Sprintf(UrlPathIDPGroups, c.scimVersion)
	ratelimitData, err := c.doRequest(
		ctx,
		http.MethodGet,
		c.getUrl(urlPathIDPGroups, map[string]interface{}{
			"startIndex": startIndex,
			"count":      count,
		}, true),
		&response,
		WithBearerToken(c.token),
	)
	if err != nil {
		return nil, ratelimitData, fmt.Errorf("error fetching IDP groups: %w", err)
	}

	return &response, ratelimitData, nil
}

// ListIDPUsers returns all IDP users from the SCIM API.
func (c *Client) ListIDPUsers(
	ctx context.Context,
	startIndex int,
	count int,
) (
	*SCIMResponse[UserResource],
	*v2.RateLimitDescription,
	error,
) {
	var response SCIMResponse[UserResource]
	urlPathIDPUsers := fmt.Sprintf(UrlPathIDPUsers, c.scimVersion)
	ratelimitData, err := c.doRequest(
		ctx,
		http.MethodGet,
		c.getUrl(urlPathIDPUsers, map[string]interface{}{
			"startIndex": startIndex,
			"count":      count,
		}, true),
		&response,
		WithBearerToken(c.token),
	)
	if err != nil {
		return nil, ratelimitData, fmt.Errorf("error fetching IDP users: %w", err)
	}

	return &response, ratelimitData, nil
}

// GetIDPGroup returns a single IDP group from the SCIM API.
func (c *Client) GetIDPGroup(
	ctx context.Context,
	groupID string,
) (
	*GroupResource,
	*v2.RateLimitDescription,
	error,
) {
	var response GroupResource
	ratelimitData, err := c.doRequest(
		ctx,
		http.MethodGet,
		c.getUrl(fmt.Sprintf(UrlPathIDPGroup, c.scimVersion, groupID), nil, true),
		&response,
		WithBearerToken(c.token),
	)
	if err != nil {
		return nil, ratelimitData, fmt.Errorf("error fetching IDP group: %w", err)
	}

	return &response, ratelimitData, nil
}

// AddUserToGroup patches a group by adding a user to it.
func (c *Client) AddUserToGroup(
	ctx context.Context,
	groupID string,
	user string,
) (
	*v2.RateLimitDescription,
	error,
) {
	group, ratelimitData, err := c.GetIDPGroup(ctx, groupID)
	if err != nil {
		return ratelimitData, fmt.Errorf("error fetching IDP group: %w", err)
	}

	var members []UserID
	for _, member := range group.Members {
		if member.Value == user {
			return ratelimitData, nil
		}
		members = append(members, UserID{Value: member.Value})
	}

	members = append(members, UserID{Value: user})

	requestBody := PatchOp{
		Schemas: []string{scimPatchOpURN},
		Operations: []ScimOperate{
			{
				Op:    "add",
				Path:  "members",
				Value: members,
			},
		},
	}

	ratelimitData, err = c.patchGroup(ctx, groupID, requestBody)
	if err != nil {
		return ratelimitData, fmt.Errorf("error adding user to IDP group: %w", err)
	}

	return ratelimitData, nil
}

// RemoveUserFromGroup patches a group by removing a user from it.
func (c *Client) RemoveUserFromGroup(
	ctx context.Context,
	groupID string,
	user string,
) (
	bool,
	*v2.RateLimitDescription,
	error,
) {
	// First, we need to fetch group to get existing members.
	group, ratelimitData, err := c.GetIDPGroup(ctx, groupID)
	if err != nil {
		return false, ratelimitData, fmt.Errorf("error fetching IDP group: %w", err)
	}

	found := false
	for _, member := range group.Members {
		if member.Value == user {
			found = true
			break
		}
	}

	// If we don't find the user, we can short-circuit here.
	if !found {
		return false, ratelimitData, nil
	}

	requestBody := PatchOp{
		Schemas: []string{scimPatchOpURN},
		Operations: []ScimOperate{
			{
				Op:   "remove",
				Path: "members",
				Value: []UserID{
					{
						Value: user,
					},
				},
			},
		},
	}

	ratelimitData, err = c.patchGroup(ctx, groupID, requestBody)
	if err != nil {
		return false, ratelimitData, fmt.Errorf("error removing user from IDP group: %w", err)
	}

	return true, ratelimitData, nil
}

func (c *Client) patchGroup(
	ctx context.Context,
	groupID string,
	requestBody PatchOp,
) (
	*v2.RateLimitDescription,
	error,
) {
	var response *GroupResource
	ratelimitData, err := c.doRequest(
		ctx,
		http.MethodPatch,
		c.getUrl(fmt.Sprintf(UrlPathIDPGroup, c.scimVersion, groupID), nil, true),
		&response,
		WithBearerToken(c.token),
		uhttp.WithJSONBody(requestBody),
	)
	if err != nil {
		return ratelimitData, fmt.Errorf("error patching IDP group: %w", err)
	}

	return ratelimitData, nil
}

type InviteUserParams struct {
	TeamID     string
	ChannelIDs string
	Email      string
}

// DisableUser deactivates a user via SCIM API using DELETE.
// https://docs.slack.dev/reference/scim-api/
func (c *Client) DisableUser(
	ctx context.Context,
	userID string,
) (
	*v2.RateLimitDescription,
	error,
) {
	var response any
	ratelimitData, err := c.doRequest(
		ctx,
		http.MethodDelete,
		c.getUrl(fmt.Sprintf(UrlPathIDPUser, c.scimVersion, userID), nil, true),
		&response,
		WithBearerToken(c.token),
	)
	if err != nil {
		return ratelimitData, fmt.Errorf("error disabling user: %w", err)
	}

	return ratelimitData, nil
}

// EnableUser activates a user via SCIM API by setting active to true.
func (c *Client) EnableUser(
	ctx context.Context,
	userID string,
) (
	*v2.RateLimitDescription,
	error,
) {
	requestBody := map[string]any{
		"schemas": []string{scimPatchOpURN},
		"Operations": []map[string]any{
			{
				"path":  "active",
				"op":    "replace",
				"value": true,
			},
		},
	}

	var response any
	ratelimitData, err := c.doRequest(
		ctx,
		http.MethodPatch,
		c.getUrl(fmt.Sprintf(UrlPathIDPUser, c.scimVersion, userID), nil, true),
		&response,
		WithBearerToken(c.token),
		uhttp.WithJSONBody(requestBody),
	)
	if err != nil {
		return ratelimitData, fmt.Errorf("error enabling user: %w", err)
	}

	return ratelimitData, nil
}

func SetWorkspaceNames(ctx context.Context, ss sessions.SessionStore, workspaces []slack.Team) error {
	workspaceMap := make(map[string]string)
	for _, workspace := range workspaces {
		workspaceMap[workspace.ID] = workspace.Name
	}
	return session.SetManyJSON(ctx, ss, workspaceMap, workspaceNameNamespace)
}

// GetWorkspaceNames retrieves workspace names for the given IDs from the session store.
func GetWorkspaceNames(ctx context.Context, ss sessions.SessionStore, workspaceIDs []string) (map[string]string, []string, error) {
	validIDs := make([]string, 0, len(workspaceIDs))
	for _, id := range workspaceIDs {
		if id != "" {
			validIDs = append(validIDs, id)
		}
	}

	if len(validIDs) == 0 {
		return make(map[string]string), []string{}, nil
	}

	found, err := session.GetManyJSON[string](ctx, ss, validIDs, workspaceNameNamespace)
	if err != nil {
		return nil, nil, err
	}

	missing := make([]string, 0)
	for _, id := range validIDs {
		if _, exists := found[id]; !exists {
			missing = append(missing, id)
		}
	}

	return found, missing, nil
}
