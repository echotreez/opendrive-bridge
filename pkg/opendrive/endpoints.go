package opendrive

// Upstream endpoint paths, relative to the API base URL.
//
// They are declared as constants for three reasons the whitepaper insists on:
// upstream's own spelling is the contract, typos included (§2.6 #3); several
// resources mean different things depending on the verb, so routing must never
// be inferred from the path (§2.6 #4); and the contract tests compare these
// literals against the archived Swagger spec (§6.1).
//
// The list covers the first-release scope (§1.4). Endpoints are bound to typed
// methods in phase P2.
const (
	// Session and OAuth2 (PDF §11, whitepaper §2.2).
	EndpointSessionLogin           = "/session/login.json"  // POST
	EndpointSessionExists          = "/session/exists.json" // POST
	EndpointSessionInfo            = "/session/info.json"   // GET
	EndpointSessionLogout          = "/session/logout.json" // POST
	EndpointSessionCaptchaRequired = "/session/captcharequired.json"
	EndpointOAuth2Grant            = "/oauth2/grant.json" // POST

	// Folder (PDF §5).
	//
	// EndpointFolder is POST to create a folder; EndpointFolderTrash is POST to
	// move a folder to the trash and DELETE to empty the trash (§2.6 #4).
	EndpointFolder           = "/folder.json"
	EndpointFolderList       = "/folder/list.json" // GET, session in the path
	EndpointFolderInfo       = "/folder/info.json"
	EndpointFolderIDByPath   = "/folder/idbypath.json"
	EndpointFolderItemByName = "/folder/itembyname.json"
	// EndpointFolderBreadcrumb keeps upstream's misspelling: the endpoint really
	// is "breadcrump.json" (§2.6 #3). Do not correct it.
	EndpointFolderBreadcrumb  = "/folder/breadcrump.json"
	EndpointFolderRename      = "/folder/rename.json"
	EndpointFolderMoveCopy    = "/folder/move_copy.json"
	EndpointFolderTrash       = "/folder/trash.json" // POST = trash, DELETE = empty
	EndpointFolderTrashList   = "/folder/trashlist.json"
	EndpointFolderRestore     = "/folder/restore.json"
	EndpointFolderRemove      = "/folder/remove.json"
	EndpointFolderSetAccess   = "/folder/setaccess.json"
	EndpointFolderSettings    = "/folder/foldersettings.json"
	EndpointFolderShared      = "/folder/shared.json"
	EndpointFolderSharedInfo  = "/folder/sharedinfo.json"
	EndpointFolderSendByEmail = "/folder/sendbyemail.json"

	// File (PDF §4).
	//
	// EndpointFile is POST to create an empty file and DELETE to remove a
	// trashed file permanently (§2.6 #4).
	EndpointFile               = "/file/file.json"
	EndpointFileInfo           = "/file/info.json"
	EndpointFileIDByPath       = "/file/idbypath.json"
	EndpointFilePath           = "/file/path.json"
	EndpointFileRename         = "/file/rename.json"
	EndpointFileMoveCopy       = "/file/move_copy.json"
	EndpointFileTrash          = "/file/trash.json"
	EndpointFileRestore        = "/file/restore.json"
	EndpointFileVersions       = "/file/fileversions.json"
	EndpointFileRemoveVersion  = "/file/removefileversion.json"
	EndpointFileThumb          = "/file/thumb.json"
	EndpointFileAccess         = "/file/access.json" // PUT
	EndpointFileSettings       = "/file/filesettings.json"
	EndpointFileVerifyPassword = "/file/verifypassword.json"
	EndpointFileSendByEmail    = "/file/sendbyemail.json"

	// Upload pipeline (PDF §12, whitepaper §2.4).
	EndpointUploadCheckFileExists = "/upload/checkfileexistsbyname.json"
	EndpointUploadCreateFile      = "/upload/create_file.json"
	EndpointUploadOpenFile        = "/upload/open_file_upload.json"
	EndpointUploadChunk           = "/upload/upload_file_chunk2.json" // v2 only, §2.4
	EndpointUploadCloseFile       = "/upload/close_file_upload.json"

	// Download (PDF §3).
	//
	// EndpointDownloadAll is the one endpoint whose session parameter the PDF
	// calls session_key; see docs/discrepancies.md (§2.6 #2).
	EndpointDownloadFile = "/download/file.json"
	EndpointDownloadAll  = "/download/all.json"

	// Users, read-only in the first release (PDF §13).
	EndpointUsersInfo = "/users/info.json"
)
