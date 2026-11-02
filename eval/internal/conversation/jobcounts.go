package conversation

import "fmt"

type JobCounts struct {
	Pending    int `json:"pending"`
	Processing int `json:"processing"`
	Completed  int `json:"completed"`
	Failed     int `json:"failed"`
	Rejected   int `json:"rejected"`
}

// CountJobs preserves failures separately from successful completions. Unknown
// kinds, states and duplicate identities make the observation untrustworthy.
func CountJobs(jobs []JobState) (map[string]JobCounts, error) {
	result := map[string]JobCounts{"episode": {}, "memory": {}}
	seen := map[string]bool{}
	for _, job := range jobs {
		c, ok := result[job.Kind]
		if !ok || job.ID == "" || job.Attempts < 0 {
			return nil, fmt.Errorf("invalid job observation")
		}
		key := job.Kind + "/" + job.ID
		if seen[key] {
			return nil, fmt.Errorf("duplicate job observation")
		}
		seen[key] = true
		switch job.Status {
		case "pending":
			c.Pending++
		case "processing":
			c.Processing++
		case "completed":
			c.Completed++
		case "failed":
			c.Failed++
		case "rejected":
			if job.Kind != "memory" {
				return nil, fmt.Errorf("invalid episode status")
			}
			c.Rejected++
		default:
			return nil, fmt.Errorf("unknown job status")
		}
		result[job.Kind] = c
	}
	return result, nil
}
