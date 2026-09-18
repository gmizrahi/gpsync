package dashboard

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// handleCleanOriginals moves queued originals-folder files into review.
//
// Takes no input: the pass decides for itself which rows qualify, so there
// is nothing client-supplied to validate. Nothing is uploaded or deleted --
// the files land in the review queue this page already serves, where each
// one is resolved individually.
func handleCleanOriginals(w http.ResponseWriter, r *http.Request, db *statedb.DB) {
	v := url.Values{}
	sum, err := engine.CleanOriginals(db, true)
	switch {
	case err != nil:
		v.Set("error", err.Error())
	case sum.Matched == 0:
		v.Set("cleaned", "Nothing queued from an originals folder. Nothing changed.")
	default:
		v.Set("cleaned", fmt.Sprintf("Moved %d file(s) into review. Nothing was uploaded or deleted.", sum.Matched))
	}
	http.Redirect(w, r, "/originals?"+v.Encode(), http.StatusSeeOther)
}
