package linear

import "context"

// Label is a label available to an Issue's Team (spec 16 3.6.5).
type Label struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

const teamLabelsQuery = `query EvaloomTeamLabels($id: String!, $first: Int!, $after: String) {
  team(id: $id) { id labels(first: $first, after: $after) {
    nodes { id name } pageInfo { hasNextPage endCursor }
  } }
}`

// TeamLabels reads every page; a failed or incomplete read must not be
// mistaken for an absent label (spec 16 Q164).
func (c *Client) TeamLabels(ctx context.Context, teamID string) ([]Label, error) {
	const op = "list team labels"
	if teamID == "" {
		return nil, &Error{Op: op, Class: ClassOther}
	}
	key, err := c.key()
	if err != nil {
		return nil, err
	}
	var all []Label
	var after *string
	seen := map[string]bool{}
	for {
		var data struct {
			Team *struct {
				ID     string `json:"id"`
				Labels *struct {
					Nodes    []Label `json:"nodes"`
					PageInfo struct {
						HasNextPage bool    `json:"hasNextPage"`
						EndCursor   *string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"labels"`
			} `json:"team"`
		}
		if err := c.do(ctx, op, key, teamLabelsQuery,
			map[string]any{"id": teamID, "first": labelPageSize, "after": after}, &data); err != nil {
			return nil, err
		}
		if data.Team == nil || data.Team.ID != teamID || data.Team.Labels == nil {
			return nil, &Error{Op: op, Class: ClassOther}
		}
		for _, label := range data.Team.Labels.Nodes {
			if label.ID == "" || label.Name == "" {
				return nil, &Error{Op: op, Class: ClassOther}
			}
			all = append(all, label)
		}
		if !data.Team.Labels.PageInfo.HasNextPage {
			return all, nil
		}
		cursor := data.Team.Labels.PageInfo.EndCursor
		if cursor == nil || *cursor == "" || seen[*cursor] {
			return nil, &Error{Op: op, Class: ClassOther}
		}
		seen[*cursor] = true
		after = cursor
	}
}

const issueAddLabelMutation = `mutation EvaloomIssueAddLabel($id: String!, $labelId: String!) {
  issueAddLabel(id: $id, labelId: $labelId) { success }
}`

// AddIssueLabel preserves all existing labels. The additive mutation and
// its String! arguments are defined in Linear's official schema:
// https://github.com/linear/linear/blob/master/packages/sdk/src/schema.graphql
func (c *Client) AddIssueLabel(ctx context.Context, issueID, labelID string) error {
	const op = "add issue label"
	if issueID == "" || labelID == "" {
		return &Error{Op: op, Class: ClassOther}
	}
	key, err := c.key()
	if err != nil {
		return err
	}
	var data struct {
		IssueAddLabel *struct {
			Success bool `json:"success"`
		} `json:"issueAddLabel"`
	}
	if err := c.do(ctx, op, key, issueAddLabelMutation,
		map[string]any{"id": issueID, "labelId": labelID}, &data); err != nil {
		return err
	}
	if data.IssueAddLabel == nil || !data.IssueAddLabel.Success {
		return &Error{Op: op, Class: ClassOther}
	}
	return nil
}
