package statedb

import (
	"path/filepath"
	"strings"
)

// likeEscaper escapes the three characters SQLite's LIKE gives special
// meaning, for use with `ESCAPE '\'`. Backslash goes first in the list
// only for readability -- strings.NewReplacer matches all patterns in a
// single left-to-right pass, so an escaped backslash is never re-escaped.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// folderLikePattern turns a folder path into a LIKE pattern matching
// exactly the files UNDER that folder. Every LIKE query in this file must
// use it (together with `ESCAPE '\'`), because naive `path + "%"` had two
// distinct over-matching bugs:
//
//   - `_` is LIKE's single-character wildcard, and this project's own
//     folder naming (`2024_01`) sits right on it -- so `.../2024_01` also
//     matched `.../2024X01`, `.../2024-01`, and so on.
//   - With no trailing separator, `C:\Photos\2024` matched the SIBLING
//     folders `C:\Photos\2024 Backup\` and `C:\Photos\2024Archive\` too. For
//     `gpsync sync --force` that means silently resetting a whole unrelated
//     folder's rows to pending and re-uploading it -- burning quota on
//     files the user never asked to touch.
//
// The separator is appended before escaping so a Windows separator (`\`)
// gets escaped along with everything else rather than being left as a
// dangling LIKE escape character.
func folderLikePattern(folder string) string {
	if !strings.HasSuffix(folder, string(filepath.Separator)) {
		folder += string(filepath.Separator)
	}
	return likeEscaper.Replace(folder) + "%"
}

func scanUpload(row interface {
	Scan(...any) error
}) (*Upload, error) {
	var u Upload
	err := row.Scan(&u.SHA256, &u.Size, &u.MimeType, &u.Status, &u.GoogleMediaItemID,
		&u.FirstSourcePath, &u.CapturedAt, &u.UploadedAt, &u.LastErrorCode,
		&u.LastErrorMessage, &u.AttemptCount)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

const uploadCols = `sha256, size, mime_type, status, google_media_item_id, first_source_path, captured_at, uploaded_at, last_error_code, last_error_message, attempt_count`

func likeAnyPrefix(column string, prefixes []string) (string, []any) {
	clause := "("
	args := make([]any, 0, len(prefixes))
	for i, p := range prefixes {
		if i > 0 {
			clause += " OR "
		}
		clause += column + ` LIKE ? ESCAPE '\'`
		args = append(args, folderLikePattern(p))
	}
	return clause + ")", args
}
