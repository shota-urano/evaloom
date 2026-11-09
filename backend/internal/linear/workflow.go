package linear

import "context"

// WorkflowState is a Team's state, selected by exact name (spec 16 3.6.2).
type WorkflowState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// IssueState is the current state and Team used by state delivery. This read
// does not depend on intake's labels or description (spec 16 3.6.3).
type IssueState struct {
	TeamID   string
	State    WorkflowState
	Archived bool
	Trashed  bool
}

const issueStateQuery = `query EvaloomIssueState($id: String!) {
  issue(id: $id) { id team { id } state { id name type } archivedAt trashed }
}`

func (c *Client) IssueState(ctx context.Context, issueID string) (IssueState, error) {
	const op = "get issue state"
	if issueID == "" {
		return IssueState{}, &Error{Op: op, Class: ClassOther}
	}
	key, err := c.key()
	if err != nil {
		return IssueState{}, err
	}
	var data struct {
		Issue *struct {
			ID   string `json:"id"`
			Team *struct {
				ID string `json:"id"`
			} `json:"team"`
			State      *WorkflowState `json:"state"`
			ArchivedAt *string        `json:"archivedAt"`
			Trashed    bool           `json:"trashed"`
		} `json:"issue"`
	}
	if err := c.do(ctx, op, key, issueStateQuery, map[string]any{"id": issueID}, &data); err != nil {
		return IssueState{}, err
	}
	if data.Issue == nil {
		return IssueState{}, ErrIssueNotFound
	}
	i := data.Issue
	if i.ID != issueID || i.Team == nil || i.Team.ID == "" || i.State == nil ||
		i.State.ID == "" || i.State.Name == "" || i.State.Type == "" {
		return IssueState{}, &Error{Op: op, Class: ClassOther}
	}
	return IssueState{TeamID: i.Team.ID, State: *i.State, Archived: i.ArchivedAt != nil, Trashed: i.Trashed}, nil
}

const teamStatesQuery = `query EvaloomTeamStates($id: String!, $first: Int!, $after: String) {
  team(id: $id) { id states(first: $first, after: $after) {
    nodes { id name type } pageInfo { hasNextPage endCursor }
  } }
}`

// TeamStates reads every page; a partial list must not cause state_not_found.
func (c *Client) TeamStates(ctx context.Context, teamID string) ([]WorkflowState, error) {
	const op = "list team states"
	if teamID == "" {
		return nil, &Error{Op: op, Class: ClassOther}
	}
	key, err := c.key()
	if err != nil {
		return nil, err
	}
	var all []WorkflowState
	var after *string
	seen := map[string]bool{}
	for {
		var data struct {
			Team *struct {
				ID     string `json:"id"`
				States *struct {
					Nodes    []WorkflowState `json:"nodes"`
					PageInfo struct {
						HasNextPage bool    `json:"hasNextPage"`
						EndCursor   *string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"states"`
			} `json:"team"`
		}
		vars := map[string]any{"id": teamID, "first": issuePageSize, "after": after}
		if err := c.do(ctx, op, key, teamStatesQuery, vars, &data); err != nil {
			return nil, err
		}
		if data.Team == nil || data.Team.ID != teamID || data.Team.States == nil {
			return nil, &Error{Op: op, Class: ClassOther}
		}
		for _, state := range data.Team.States.Nodes {
			if state.ID == "" || state.Name == "" || state.Type == "" {
				return nil, &Error{Op: op, Class: ClassOther}
			}
			all = append(all, state)
		}
		if !data.Team.States.PageInfo.HasNextPage {
			return all, nil
		}
		cursor := data.Team.States.PageInfo.EndCursor
		if cursor == nil || *cursor == "" || seen[*cursor] {
			return nil, &Error{Op: op, Class: ClassOther}
		}
		seen[*cursor] = true
		after = cursor
	}
}

const issueStateMutation = `mutation EvaloomIssueSetState($id: String!, $stateId: String!) {
  issueUpdate(id: $id, input: { stateId: $stateId }) { success }
}`

// SetIssueState only changes stateId; repeating it is idempotent.
func (c *Client) SetIssueState(ctx context.Context, issueID, stateID string) error {
	const op = "set issue state"
	if issueID == "" || stateID == "" {
		return &Error{Op: op, Class: ClassOther}
	}
	key, err := c.key()
	if err != nil {
		return err
	}
	var data struct {
		IssueUpdate *struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	if err := c.do(ctx, op, key, issueStateMutation, map[string]any{"id": issueID, "stateId": stateID}, &data); err != nil {
		return err
	}
	if data.IssueUpdate == nil || !data.IssueUpdate.Success {
		return &Error{Op: op, Class: ClassOther}
	}
	return nil
}
