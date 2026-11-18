package memory

import "testing"

// The shapes measured in the logs (paraphrased): clause-initial "remember"
// is an ask; "remember" inside a clause is not.
func TestUserRememberRx_MeasuredShapes(t *testing.T) {
	for _, s := range []string{
		"Remember we don't want the load test to send email.",
		"remember our goals here: realistic physics",
		"Continue with it. Remember, once a sprint is picked it is closed.",
		"And remember, a project without isolation tests stays single-worktree.",
		"Can you write a script for this? remember, we have several aws accounts.",
		"Keep in mind the API is rate limited.",
		"From now on, run the linter before committing.",
		"Don't forget: staging uses a different region.",
	} {
		if !userRememberRx.MatchString(s) {
			t.Errorf("missed: %q", s)
		}
	}
	for _, s := range []string{
		"I remember absolute drift had a good UI for this.",
		"they don't remember sending a password",
		"we don't want personalization to pollute, but we want to remember it",
		"the ability to remember user decisions",
		"What do you remember about the parser?",
	} {
		if userRememberRx.MatchString(s) {
			t.Errorf("fired on: %q", s)
		}
	}
}

func TestAssistantClaimRx(t *testing.T) {
	for _, s := range []string{
		"Understood. I'll remember that.",
		"I've saved that as a standing rule.",
		"I have recorded the toolchain path for future sessions.",
	} {
		if !assistantClaimRx.MatchString(s) {
			t.Errorf("missed: %q", s)
		}
	}
	for _, s := range []string{
		"I've noted it in the plan.",
		"I've noted that in the commit so it's on the record.",
		"I'll note it for its owner.",
	} {
		if assistantClaimRx.MatchString(s) {
			t.Errorf("fired on: %q", s)
		}
	}
}
