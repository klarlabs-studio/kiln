// Package forge holds what kiln needs to know about the code-hosting service
// a repository lives on — GitHub today, and nothing in this package assumes
// that stays true.
//
// It sits in the domain because the isolation policy is decided from it. Which
// HTTP call produced a Pull is an adapter's business; whether the head lives in
// somebody else's repository is not.
package forge

// Pull is what Kiln needs to know about a pull request.
type Pull struct {
	Number int
	// HeadSHA is the commit to build.
	HeadSHA string
	// HeadRef is the source branch name.
	HeadRef string
	// Fork reports that the head lives in a different repository. This is the
	// input to the isolation policy, and getting it wrong in the permissive
	// direction hands a stranger the operator's credentials — so callers that
	// cannot reach the API must assume true, not false.
	Fork bool
	// Draft pull requests are still proven; the flag is reported so a caller
	// can choose to skip them.
	Draft bool
}
