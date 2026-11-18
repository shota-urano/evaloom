package linear

import "context"

// teamPageSize is request shaping, not an Evaloom setting.
const teamPageSize = 50

// Team is the subset of a Linear Team used to find the polling Team (spec 15
// 3.2, Q216).
type Team struct {
	ID   string
	Key  string
	Name string
}

const teamsQuery = `query EvaloomTeams($first: Int!, $after: String) {
  teams(first: $first, after: $after) {
    nodes { id key name }
    pageInfo { hasNextPage endCursor }
  }
}`

// ListTeams returns every Team the API key can see, reading all pages (spec
// 15 3.2 and 5章: the Team list is read through this client). If any page
// fails, no list is returned: a partial list could hide a second matching
// Team or the matching Team itself.
func (c *Client) ListTeams(ctx context.Context) ([]Team, error) {
	const op = "list teams"
	key, err := c.key()
	if err != nil {
		return nil, err
	}
	var all []Team
	var after *string
	seen := map[string]bool{}
	for {
		var data struct {
			Teams *struct {
				Nodes []struct {
					ID   string `json:"id"`
					Key  string `json:"key"`
					Name string `json:"name"`
				} `json:"nodes"`
				PageInfo struct {
					HasNextPage bool    `json:"hasNextPage"`
					EndCursor   *string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"teams"`
		}
		vars := map[string]any{"first": teamPageSize, "after": after}
		if err := c.do(ctx, op, key, teamsQuery, vars, &data); err != nil {
			return nil, err
		}
		if data.Teams == nil {
			return nil, &Error{Op: op, Class: ClassOther}
		}
		for _, node := range data.Teams.Nodes {
			if node.ID == "" {
				return nil, &Error{Op: op, Class: ClassOther}
			}
			all = append(all, Team{ID: node.ID, Key: node.Key, Name: node.Name})
		}
		if !data.Teams.PageInfo.HasNextPage {
			return all, nil
		}
		cursor := data.Teams.PageInfo.EndCursor
		// A missing or repeated cursor would loop forever or skip pages.
		if cursor == nil || *cursor == "" || seen[*cursor] {
			return nil, &Error{Op: op, Class: ClassOther}
		}
		seen[*cursor] = true
		after = cursor
	}
}
