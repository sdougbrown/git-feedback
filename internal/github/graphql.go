package github

import (
	"fmt"
	"time"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

// graphqlPageSize is the pinned page size for GraphQL and REST pagination.
const graphqlPageSize = 100

// gqlRateLimit is the rateLimit node requested alongside every read query.
type gqlRateLimit struct {
	Remaining int       `json:"remaining"`
	Limit     int       `json:"limit"`
	ResetAt   time.Time `json:"resetAt"`
}

func (r *gqlRateLimit) rateInfo() forge.RateInfo {
	return forge.RateInfo{
		Resource:  ResourceGraphQL,
		Remaining: r.Remaining,
		Limit:     r.Limit,
		Reset:     r.ResetAt,
	}
}

// gqlPageInfo is standard Relay pagination metadata.
type gqlPageInfo struct {
	HasNextPage bool    `json:"hasNextPage"`
	EndCursor   *string `json:"endCursor"`
}

type gqlCommentNode struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	Author    *struct {
		Login string `json:"login"`
	} `json:"author"`
}

type gqlThreadNode struct {
	ID         string `json:"id"`
	IsOutdated bool   `json:"isOutdated"`
	IsResolved bool   `json:"isResolved"`
	Path       string `json:"path"`
	Comments   struct {
		TotalCount int              `json:"totalCount"`
		PageInfo   gqlPageInfo      `json:"pageInfo"`
		Nodes      []gqlCommentNode `json:"nodes"`
	} `json:"comments"`
}

type gqlThreadsData struct {
	Repository struct {
		PullRequest struct {
			ReviewThreads struct {
				TotalCount int             `json:"totalCount"`
				PageInfo   gqlPageInfo     `json:"pageInfo"`
				Nodes      []gqlThreadNode `json:"nodes"`
			} `json:"reviewThreads"`
		} `json:"pullRequest"`
	} `json:"repository"`
	RateLimit *gqlRateLimit `json:"rateLimit"`
}

type gqlThreadCommentsData struct {
	Node struct {
		Comments struct {
			TotalCount int              `json:"totalCount"`
			PageInfo   gqlPageInfo      `json:"pageInfo"`
			Nodes      []gqlCommentNode `json:"nodes"`
		} `json:"comments"`
	} `json:"node"`
	RateLimit *gqlRateLimit `json:"rateLimit"`
}

const threadsQuery = `query($owner:String!,$name:String!,$number:Int!,$cursor:String){
  repository(owner:$owner,name:$name){
    pullRequest(number:$number){
      reviewThreads(first:100,after:$cursor){
        totalCount
        pageInfo{hasNextPage endCursor}
        nodes{id isOutdated isResolved path
          comments(first:100){totalCount pageInfo{hasNextPage endCursor} nodes{id body createdAt author{login}}}
        }
      }
    }
  }
  rateLimit{remaining limit resetAt}
}`

const threadCommentsQuery = `query($id:ID!,$cursor:String){
  node(id:$id){
    ... on PullRequestReviewThread{
      comments(first:100,after:$cursor){totalCount pageInfo{hasNextPage endCursor} nodes{id body createdAt author{login}}}
    }
  }
  rateLimit{remaining limit resetAt}
}`

// rateLimitFromData extracts and records the rateLimit node, enforcing the
// reserve when fewer than ReserveRemaining points remain.
func (a *Adapter) recordGQLRateLimit(rl *gqlRateLimit) error {
	if rl == nil {
		return fmt.Errorf("%w: GraphQL response missing rateLimit", forge.ErrIncomplete)
	}
	a.gate.Record(rl.rateInfo())
	if rl.Remaining > 0 && rl.Remaining < ReserveRemaining {
		return &forge.ErrRateLimited{Resource: ResourceGraphQL, Until: rl.ResetAt}
	}
	return nil
}
