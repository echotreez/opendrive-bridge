<?php
// OpenDrive API
// Get user info
// https://dev.opendrive.com/api/explorer/#!/users/retrieveInfo_get

// The data to send to the API
$sessionID = ''; //string required Session ID.

// Setup Url
$url = 'https://dev.opendrive.com/api/v1/users/info.json/' . $sessionID;
$response = file_get_contents($url);

// Decode the response
$responseData = json_decode($response, TRUE);

print_r($responseData);
