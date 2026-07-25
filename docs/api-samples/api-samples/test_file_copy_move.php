<?php
// OpenDrive API
// Copy or move a file
// https://dev.opendrive.com/api/explorer/#!/file/createMove_copy_post

// The data to send to the API
$postData = array(
    'session_id'           => '',                //string (required) - Session ID.
    'src_file_id'          => '',                //string (required) - Source file ID.
    'dest_folder_id'       => '',                //string (required) - Destination folder ID.
    'move'                 => '',                //string (required) - (true = move, false = copy).
    'overwrite_if_exists'  => '',                //string (required) - (true, false).
    'src_access_folder_id' => '',                //string - Source access folder.
    'dst_access_folder_id' => ''                 //string - Destination access folder.
);

// Setup cURL
$ch = curl_init('https://dev.opendrive.com/api/v1/file/move_copy.json');
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
curl_close($ch);

// Decode the response
$responseData = json_decode($response, TRUE);

print_r($responseData);