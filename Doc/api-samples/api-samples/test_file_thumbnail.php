<?php
// OpenDrive API
// File thumbnail
// https://dev.opendrive.com/api/explorer/#!/file/retrieveThumb_get

// The data to send to the API
$sessionID = '';            //string required Session ID.
$fileID = '';               //string required File ID.

// Setup Url
$url = 'https://dev.opendrive.com/api/v1/file/thumb.json/' . $fileID . '?session_id=' . $sessionID;

$response = file_get_contents($url);

echo $response; //binary image data
