<?php
// OpenDrive API
// Update user info
// https://dev.opendrive.com/api/explorer/#!/users/updateInfo_put

// The data to send to the API
$postData = array(
    'session_id'      => '',            //string (required) - Session ID.
    'first_name'      => '',            //string - First Name (min 2) (max 50).
    'last_name'       => '',            //string - Last Name (min 2) (max 50).
    'company_name'    => '',            //string - Company Name (max 255).
    'phone'           => '',            //string - Phone (max 20).
    'time_zone'       => '',            //string - Time Zone database version 2013.6.
    'file_versioning' => '',            //int - File Versioning (0 = off, 1=on).
    'file_versions'   => '',            //int - File Versions (max 99).
    'lang'            => '',            //string - (ISO 2 Letter Language Codes).
    'daily_stat'      => '',            // int - Daily Stats (0 = off, 1=on).
);

// Setup cURL
$ch = curl_init('http://dev.opendrive.com/api/v1/users/info.json');
curl_setopt_array($ch, array(
    CURLOPT_CUSTOMREQUEST  => 'PUT',
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
