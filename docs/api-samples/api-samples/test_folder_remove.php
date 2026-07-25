<?php
// OpenDrive API
// Delete a folder from trash
// https://dev.opendrive.com/api/explorer/#!/folder/createRemove_post

// The data to send to the API
$postData = array(
    'session_id' => '',        // string (required) - Session ID.
    'folder_id'  => ''        // string (required) - Folder ID.
);

// Setup cURL
$ch = curl_init('https://dev.opendrive.com/api/v1/folder/remove.json');
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