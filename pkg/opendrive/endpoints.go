package opendrive

// Upstream endpoint paths, relative to the API base URL.
//
// Every constant here was taken from the archived live Swagger specification in
// testdata/spec/, not from the PDF (CLAUDE.md rule 3). TestEndpointConstants-
// MatchTheArchivedSpec re-checks each one against the archive, so a rename
// upstream fails CI instead of failing in production (§6.1, §12.2).
//
// Three things are deliberately explicit:
//   - the verb, because several resources mean different things per method
//     (§2.6 #4): POST /file.json creates an empty file while DELETE removes a
//     trashed one, and POST /folder/trash.json trashes while DELETE empties;
//   - where the session goes, because upstream puts it in the body, the query
//     or the path depending on the endpoint;
//   - the spelling, because upstream's is the contract (§2.6 #3).
//
// The list covers the first-release scope (§1.4). Endpoints are bound to typed
// methods in phase P2.
const (
	// -------------------------------------------------- session and OAuth2
	// PDF §11, whitepaper §2.2.

	EndpointSessionLogin  = "/session/login.json"  // POST, no session
	EndpointSessionExists = "/session/exists.json" // POST, session in body
	// EndpointSessionInfo takes the session as a path segment.
	EndpointSessionInfo   = "/session/info.json"   // GET  /{session_id}
	EndpointSessionLogout = "/session/logout.json" // POST, session in body
	// EndpointSessionCaptchaRequired exists online but not in the PDF
	// (§2.6 #1, docs/discrepancies.md D2).
	EndpointSessionCaptchaRequired = "/session/captcharequired.json" // GET, no session

	EndpointOAuth2Grant = "/oauth2/grant.json" // POST, no session

	// -------------------------------------------------- folder
	// PDF §5.

	EndpointFolder = "/folder.json" // POST creates a folder, session in body
	// EndpointFolderList is the workhorse listing endpoint: session and folder
	// id are both path segments, paging goes through offset plus
	// last_request_time (§2.6 #14).
	EndpointFolderList       = "/folder/list.json"       // GET  /{session_id}/{folder_id}
	EndpointFolderInfo       = "/folder/info.json"       // GET  /{session_id}/{folder_id}
	EndpointFolderIDByPath   = "/folder/idbypath.json"   // POST, session in body
	EndpointFolderItemByName = "/folder/itembyname.json" // GET /{session_id}/{folder_id}
	// EndpointFolderBreadcrumb: the live API spells this correctly. The PDF's
	// "breadcrump" returns 404 (docs/discrepancies.md D12).
	EndpointFolderBreadcrumb  = "/folder/breadcrumb.json"     // GET /{session_id}/{folder_id}
	EndpointFolderPath        = "/folder/path.json"           // GET /{session_id}/{folder_id}
	EndpointFolderFullPath    = "/folder/folderfullpath.json" // GET /{session_id}/{folder_id}
	EndpointFolderRename      = "/folder/rename.json"         // POST
	EndpointFolderMoveCopy    = "/folder/move_copy.json"      // POST
	EndpointFolderTrash       = "/folder/trash.json"          // POST trashes; DELETE /{session_id} empties
	EndpointFolderTrashList   = "/folder/trashlist.json"      // GET  /{session_id}
	EndpointFolderRestore     = "/folder/restore.json"        // POST
	EndpointFolderRemove      = "/folder/remove.json"         // POST, permanent
	EndpointFolderSetAccess   = "/folder/setaccess.json"      // POST
	EndpointFolderSettings    = "/folder/foldersettings.json" // PUT, not POST (D15)
	EndpointFolderUserAccess  = "/folder/useraccessmode.json" // GET  /{session_id}/{folder_id}
	EndpointFolderSendByEmail = "/folder/sendbyemail.json"    // POST
	EndpointFolderExportCSV   = "/folder/exportcsv.json"      // GET  /{session_id}/{folder_id}
	EndpointFolderShared      = "/folder/shared.json"         // GET  /{folder_id}, public
	EndpointFolderSharedInfo  = "/folder/sharedinfo.json"     // GET  /{folder_id}, public
	// Expiring links pass every argument as a path segment, session first.
	EndpointFolderExpiringLink  = "/folder/expiringlink.json"        // GET /{session_id}/{date}/{counter}/{folder_id}/{enable}
	EndpointFolderExpiringLinks = "/folder/folderexpiringlinks.json" // GET /{session_id}/{folder_id}

	// -------------------------------------------------- file
	// PDF §4.

	// EndpointFile is the file resource itself: POST creates an empty file,
	// DELETE removes a trashed one. Note it is /file.json, not the
	// /file/file.json the whitepaper lists (D16).
	EndpointFile = "/file.json" // POST; DELETE /{session_id}/{file_id}
	// EndpointFileInfo takes the file id in the path and the session in the
	// query string.
	EndpointFileInfo           = "/file/info.json"              // GET  /{file_id}
	EndpointFileIDByPath       = "/file/idbypath.json"          // POST
	EndpointFilePath           = "/file/path.json"              // GET  /{session_id}/{file_id}
	EndpointFileFullPath       = "/file/filefullpath.json"      // GET  /{session_id}/{file_id}
	EndpointFileRename         = "/file/rename.json"            // POST
	EndpointFileMoveCopy       = "/file/move_copy.json"         // POST, move/overwrite are "true"/"false" strings (§2.6 #13)
	EndpointFileTrash          = "/file/trash.json"             // POST
	EndpointFileRestore        = "/file/restore.json"           // POST
	EndpointFileRemove         = "/file/remove.json"            // POST, permanent
	EndpointFileVersions       = "/file/fileversions.json"      // GET  /{session_id}/{file_group_id}
	EndpointFileRemoveVersion  = "/file/removefileversion.json" // DELETE /{session_id}/{file_id}
	EndpointFileThumb          = "/file/thumb.json"             // GET  /{file_id}
	EndpointFileAccess         = "/file/access.json"            // POST, not PUT (D15)
	EndpointFileSettings       = "/file/filesettings.json"      // PUT, not POST (D15)
	EndpointFileVerifyPassword = "/file/verifypassword.json"    // POST, may demand a captcha (D9)
	EndpointFileSendByEmail    = "/file/sendbyemail.json"       // POST
	EndpointFileExpiringLink   = "/file/expiringlink.json"      // GET /{session_id}/{date}/{counter}/{file_id}/{enable}
	EndpointFileExpiringLinks  = "/file/fileexpiringlinks.json" // GET /{session_id}/{file_id}

	// -------------------------------------------------- upload
	// PDF §12, whitepaper §2.4.

	EndpointUploadCheckFileExists = "/upload/checkfileexistsbyname.json" // POST /{folder_id}, name is an array (§2.6 #2)
	EndpointUploadCreateFile      = "/upload/create_file.json"           // POST
	EndpointUploadOpenFile        = "/upload/open_file_upload.json"      // POST
	// EndpointUploadChunk is the v2 chunk endpoint the official samples
	// mandate; session and file id are path segments, temp_location,
	// chunk_offset and chunk_size are query parameters, the bytes go in a
	// multipart file_data field (§2.4).
	EndpointUploadChunk = "/upload/upload_file_chunk2.json" // POST /{session_id}/{file_id}
	// EndpointUploadChunkV1 still exists upstream but is unreliable under
	// speed limits; kept only so drift detection does not flag it as new.
	EndpointUploadChunkV1   = "/upload/upload_file_chunk.json" // POST, do not use
	EndpointUploadCloseFile = "/upload/close_file_upload.json" // POST
	// EndpointUploadHasDedupeRef is undocumented in the PDF: it answers
	// whether the server already holds a blob with this size and MD5, which is
	// the dedupe probe the upload pipeline wants (D13).
	EndpointUploadHasDedupeRef = "/upload/has_ddref.json" // POST

	// -------------------------------------------------- download
	// PDF §3.

	EndpointDownloadFile = "/download/file.json" // GET  /{file_id}, supports offset resume
	// EndpointDownloadAll takes session_id in the body per the live spec, not
	// the session_key the PDF specifies (D1).
	EndpointDownloadAll      = "/download/all.json"                       // POST
	EndpointDownloadRedirect = "/download/redirecttocustomapiserver.json" // GET

	// -------------------------------------------------- sharing
	// PDF §9. The module is invisible to anonymous callers, which is why the
	// public archive appeared to lack it (D10).

	// EndpointSharing is another multi-verb resource: POST shares, DELETE
	// revokes (§2.6 #4).
	EndpointSharing                = "/sharing.json"                         // POST; DELETE /{session_id}/{sharing_id}
	EndpointSharingSetMode         = "/sharing/setmode.json"                 // PUT
	EndpointSharingListFolders     = "/sharing/listsharedfolders.json"       // GET /{session_id}/{sharing_id}
	EndpointSharingListUsers       = "/sharing/listsharedusers.json"         // GET /{session_id}
	EndpointSharingListFolderUsers = "/sharing/listusers.json"               // GET /{session_id}/{folder_id}
	EndpointSharingCheckAccess     = "/sharing/checkaccountusersaccess.json" // GET

	// -------------------------------------------------- users
	// PDF §13, read-only in the first release.

	EndpointUsersInfo = "/users/info.json" // GET /{session_id}
)
