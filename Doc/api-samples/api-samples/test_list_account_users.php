<?php
// OpenDrive API
// List an account users in group
// https://dev.opendrive.com/api/explorer/#!/accountusers/retrieveUsersInGroup_get

// The data to send to the API
$sessionID = '';              //string required Session ID.
$groupID = '';                //string required Group ID.

// Setup Url
$url = 'https://dev.opendrive.com/api/v1/accountusers/usersingroup.json/' . $sessionID . '/' . $groupID;

$response = file_get_contents($url);

// Decode the response
$responseData = json_decode($response, TRUE);

print_r($responseData);
