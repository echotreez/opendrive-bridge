<?php
// OpenDrive API
// Update folder access permission
// https://dev.opendrive.com/api/explorer/#!/folder/createSetAccess_post

// The data to send to the API
$postData = array(
    'session_id'       => '',            //string (required) - Session ID.
    'folder_id'        => '',            //string (required) - Folder ID.
    'folder_is_public' => ''             //int (required) - (0 = private, 1 = public, 2 = hidden).
);

// Setup cURL
$ch = curl_init('https://dev.opendrive.com/api/v1/folder/setaccess.json');
curl_setopt_array($ch, array(
    CURLOPT_POST           => TRUE,
    CURLOPT_RETURNTRANSFER => TRUE,
    CURLOPT_HTTPHEADER     => array(
        'Content-Type: application/json'
    ),
    CURLOPT_POSTFIELDS     => json_encode($postData)
));

// Send the request
$response = curl_exec($ch);

// Check for errors
if ($response === FALSE) {
    die(curl_error($ch));
}

// Decode the response
$responseData = json_decode($response, TRUE);

print_r($responseData);
