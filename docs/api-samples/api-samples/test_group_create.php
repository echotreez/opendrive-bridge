<?php
// OpenDrive API
// Create a group
// https://dev.opendrive.com/api/explorer/#!/usergroups/create_post

// The data to send to the API
$postData = array(
    'session_id'        => '',          //string (required) - Session ID.
    'group_name'        => '',          //string (required) - Group name (max 100).
    'group_bw_max'      => '',          //int (required) - Storage size in MB.
    'group_max_storage' => ''           //int (required) - Bandwidth size in MB.
);

// Setup cURL
$ch = curl_init('https://dev.opendrive.com/api/v1/usergroups.json');
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