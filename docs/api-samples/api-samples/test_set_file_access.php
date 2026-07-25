<?php
// OpenDrive API
// Update file access permission
// https://dev.opendrive.com/api/explorer/#!/file/createAccess_post

// The data to send to the API
$postData = array(
    'session_id'       => '',        //string (required) - Session ID.
    'file_id'          => '',        //string (required) - File ID.
    'file_ispublic'    => '',        //int (required) - (0 = private, 1 = public, 2 = hidden).
    'access_folder_id' => ''         //string - Access folder ID.
);

// Setup cURL
$ch = curl_init('https://dev.opendrive.com/api/v1/file/access.json');
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
