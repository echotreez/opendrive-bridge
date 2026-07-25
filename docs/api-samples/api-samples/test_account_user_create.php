<?php
// OpenDrive API
// Create an account user
// https://dev.opendrive.com/api/explorer/#!/accountusers/create_post

// The data to send to the API
$postData = array(
    'session_id'             => '',                //string (required) - Session ID.
    'access_first_name'      => '',                //string (required) - Access user first name (min 2)(max 50).
    'access_last_name'       => '',                //string (required) - Access user last name (min 2)(max 50).
    'access_password'        => '',                //string (required) - Access user password (min 5).
    'access_email'           => '',                //string (required) - Valid email format required(max 255).
    'access_admin_mode'      => '',                //int (required) - Access user admin mode mode (0 =users, 1=admin, 2=files).
    'access_notification'    => '',                //int (required) - Access user notification (0 = off, 1=on).
    'access_max_storage'     => '',                //int (required) - Access user max storage size in MB.
    'access_bw_max'          => '',                //int (required) - Access user max bandwidth size in MB.
    'group_id'               => '',                //string (required) - Group id.
    'access_phone'           => '',                //string - Access phone.
    'access_password_change' => '',                //int - Allow user to change password (0 = off, 1=on).
    'access_position'        => '',                //string - Access position.
    'access_send_password'   => ''                 //int - Send password by email(0 = off, 1=on)
);

// Setup cURL
$ch = curl_init('https://dev.opendrive.com/api/v1/accountusers.json');
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