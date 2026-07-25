<?php
// OpenDrive API
// Create an account user
// https://dev.opendrive.com/api/explorer/#!/users/create_post

// The data to send to the API
$postData = array(
    'username'         => '',        //string (required) - User name (min 4)(max 100).
    'passwd'           => '',        //string (required) - Password (min 5).
    'verify_passwd'    => '',        //string (required) - Verify password (min 5).
    'email'            => '',        //string (required) - Valid email format required (max 255).
    'first_name'       => '',        //string (required) - First name (min 2)(max 50).
    'last_name'        => '',        //string (required) - Last name (min 2)(max 50).
    'reg_ip'           => '',        //string (required) - (IP in format 0.0.0.0).
    'company_name'     => '',        //string - Company name (max 255).
    'phone'            => '',        //string - Phone (max 20).
    'lang'             => '',        //string - (ISO 2 Letter Language Codes).
    'session_id'       => '',        //string - Session ID(Not required if $created_by_id are provided)
    'created_by_id'    => ''         //string - Partner username
);

// Setup cURL
$ch = curl_init('https://dev.opendrive.com/api/v1/users.json');

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