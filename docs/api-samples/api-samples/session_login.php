<?php
// OpenDrive API
// Create a session
// https://dev.opendrive.com/api/explorer/#!/session/createLogin_post

// The data to send to the API
$postData = array(
    'username'   => '',            //string (required) - Username.
    'passwd'     => '',            //string (required) - User password.
    'version'    => '10',          //string - Application version number (max 10).
    'partner_id' => ''             //string - Partner username  (Empty for OpenDrive)
);

// Setup cURL
$ch = curl_init('https://dev.opendrive.com/api/v1/session/login.json');
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
