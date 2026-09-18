package dashboard

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// handleFixDates backfills capture dates from file paths.
//
// Takes no input: the pass decides for itself which rows disagree, so
// there is nothing client-supplied to validate. It only rewrites what
// gpsync recorded -- the files themselves are never touched.
func handleFixDates(w http.ResponseWriter, r *http.Request, db *statedb.DB) {
	v := url.Values{}
	sum, err := engine.FixDates(db, false)
	if err != nil {
		v.Set("error", err.Error())
	} else if sum.Fixed == 0 {
		v.Set("fixed", "Every capture date already agrees with its path. Nothing changed.")
	} else {
		v.Set("fixed", fmt.Sprintf("Corrected %d capture date(s). The files on disk were not touched.", sum.Fixed))
	}
	http.Redirect(w, r, "/statistics?"+v.Encode(), http.StatusSeeOther)
}
