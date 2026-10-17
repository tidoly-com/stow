package immich

import "github.com/tidoly-com/stow/internal/library"

// ErrSystemFolder is a folder that holds the system or programs rather than someone's photos
// (library.SystemFolder).
type ErrSystemFolder struct{ Folder string }

func (e ErrSystemFolder) Error() string { return "a system folder: " + e.Folder }

var (
	systemFolder = library.SystemFolder
	passOver     = library.PassOver
)

// tooSmall is a picture under this many bytes: an icon or a piece of a web page, not a photo. A variable
// for the tests, whose pictures are tiny.
var tooSmall int64 = 10 << 10
