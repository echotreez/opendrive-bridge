<?php
// OpenDrive API
// Copy/Move folder
// https://dev.opendrive.com/api/explorer/#!/folder/createMove_copy_post

// The data to send to the API
$postData = array(
    'session_id'    => '',          //string (required) - Session ID.
    'folder_id'     => '',          //string (required) - Source folder ID.
    'dst_folder_id' => '',          //string (required) - Destination folder ID.
    'move'          => ''           //string - (true = move, false = copy).
);

// Setup cURL
$ch = curl_init('https://dev.opendrive.com/api/v1/folder/move_copy.json');
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