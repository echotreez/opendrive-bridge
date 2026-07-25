<?php
// OpenDrive API
// List folder content
// https://dev.opendrive.com/api/explorer/#!/folder/retrieveList_get

// The data to send to the API
$sessionID = '';           //string required Session ID.
$folderID = '';            //string required Folder ID.

// Setup Url
$url = 'https://dev.opendrive.com/api/v1/folder/list.json/' . $sessionID . '/' . $folderID;

$response = file_get_contents($url);

// Decode the response
$responseData = json_decode($response, TRUE);

print_r($responseData);
