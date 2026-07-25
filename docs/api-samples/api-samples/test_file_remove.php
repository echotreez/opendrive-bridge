<?php
// OpenDrive API
// Delete a file from trash
// https://dev.opendrive.com/api/explorer/#!/file/remove_delete

// The data to send to the API
$sessionID = '';            //string required Session ID.
$fileID = '';                //string required File ID.
$accessFolderId = '';        //string Access folder ID.

// Setup Url
$url = 'https://dev.opendrive.com/api/v1/file.json/' . $sessionID . '/' . $fileID;
if ($accessFolderId != '') $url .= '?access_folder_id=' . $accessFolderId;

$ch = curl_init($url);
curl_setopt_array($ch, array(
    CURLOPT_CUSTOMREQUEST  => 'DELETE',
    CURLOPT_RETURNTRANSFER => TRUE
));


$response = curl_exec($ch);

// Decode the response
$responseData = json_decode($response, TRUE);

print_r($responseData);