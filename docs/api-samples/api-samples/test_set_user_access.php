<?php
// OpenDrive API
// Set folder access for account users
// https://dev.opendrive.com/api/explorer/#!/accountusers/createSetFolderAccess_post

$rootFoldersArray = array(
    'folder_id_1' => '0', //access_mode - int - (0 = view, 1 = edit, 2 = cancel - disable access)
    'folder_id_2' => '0'
);

// The data to send to the API
$postData = array(
    'session_id'   => '',                  //string (required) - Session ID.
    'access_email' => '',                  //string (required) - Valid email format required(max 255).
    'foldersObj'   => $rootFoldersArray    //Object (required) - Array of root folders and access to them.
);

// Setup cURL
$ch = curl_init('https://dev.opendrive.com/api/v1/accountusers/setfolderaccess.json');
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
