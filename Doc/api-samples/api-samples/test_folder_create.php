<?php
// OpenDrive API
// Create Folder
// https://dev.opendrive.com/api/explorer/#!/folder/create_post

// The data to send to the API

$postData = array(
    'session_id'            => '',    //string (required) - Session ID.
    'folder_name'           => '',    //string (required) - Folder name Valid folder name required (max 255).
    'folder_sub_parent'     => '',    //string (required) - Folder sub parent(folder_id, 0 - for root folder).
    'folder_is_public'      => '',    //int (required) - (0 = private, 1 = public, 2 = hidden).
    'folder_public_upl'     => '',    //int - Public upload (0 = disabled, 1 = enabled).
    'folder_public_display' => '',    //int - Public display (0 = disabled, 1 = enabled).
    'folder_public_dnl'     => '',    //int - Public download (0 = disabled, 1 = enabled).
    'folder_description'    => ''     //string - Folder description.
);

// Setup cURL
$ch = curl_init('https://dev.opendrive.com/api/v1/folder.json');
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