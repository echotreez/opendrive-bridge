<?php
// OpenDrive API
// Update an account user
// https://dev.opendrive.com/api/explorer/#!/accountusers/update_put

// The data to send to the API
$postData = array(
    'session_id'             => '',                  //string (required) - Session ID.
    'access_email'           => '',                  //string (required) - Valid email format required(max 255).
    'new_access_email'       => '',                  //string (required) - Valid email format required(max 255).
    'access_first_name'      => '',                  //string - Access user first name (min 2)(max 50).
    'access_last_name'       => '',                  //string - Access user last name (min 2)(max 50).
    'access_password'        => '',                  //string - Access user password (min 5).
    'access_admin_mode'      => '',                  //int - Access user admin mode (0 =users, 1=admin, 2=files).
    'access_notification'    => '',                  //int - Access user notification (0 = off, 1=on).
    'access_password_change' => '',                  //int - Allow user to change password (0 = off, 1=on).
    'access_max_storage'     => '',                  //int - Access user max storage size in MB.
    'access_bw_max'          => '',                  //int - Access user max bandwidth size in MB.
    'access_position'        => '',                  //string - Access user position.
);

// Setup cURL
$ch = curl_init('https://dev.opendrive.com/api/v1/accountusers.json');
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