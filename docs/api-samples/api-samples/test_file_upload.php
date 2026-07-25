<?php
error_reporting(-1);
// OpenDrive API
// Create a session and upload file with resumable.js library
// https://dev.opendrive.com/api/explorer/#!/session/createLogin_post
// https://dev.opendrive.com/api/explorer/#!/upload/createResumable_post
// http://resumablejs.com/

// GET SESSION ID
// The data to send to the API
$postData = array(
    'username'   => '',            //string (required) - Username.
    'passwd'     => '',            //string (required) - User password.
    'version'    => '',            //string - Application version number (max 10).
    'partner_id' => ''             //string - Partner username  (Empty for OpenDrive)
);

$folder_id = '';                   //string (required) - Folder ID. 0 for root folder.

define('API_SERVER', 'https://dev.opendrive.com/api/');

function checkResponse($ch, $responseData, $step)
{
    $code = curl_getinfo($ch, CURLINFO_HTTP_CODE);
    if (isset($responseData['error']) || $code != 200) {
        $message = isset($responseData['error']['message']) ? $responseData['error']['message'] : 'Upload error';
        echo "$step error: $code $message";
        exit;
    }
}

// Setup cURL
$ch = curl_init(API_SERVER . 'v1/session/login.json');
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
checkResponse($ch, $responseData, 'session_login');
$session_id = $responseData['SessionID'];


if (isset($_FILES) && sizeof($_FILES) > 0 && $_FILES['test_file']['error'] == 0) {
    $file_name = $_FILES['test_file']['name'];
    $file_size = $_FILES['test_file']['size'];
    $file_temp_location = $_FILES['test_file']['tmp_name'];

    // CREATE FILE
    // The data to send to the API
    $postDataCreate = array(
        'session_id'       => $session_id,        //string (required) - Session ID.
        'folder_id'        => $folder_id,            //string (required) - Folder ID.
        'file_name'        => $file_name,            //string (required) - File Name.
        'file_size'        => $file_size,            //string (required) - File Size in bytes.
        'access_folder_id' => ''                //string - Access folder ID.
    );

    // Setup cURL
    $ch_create = curl_init(API_SERVER . 'v1/upload/create_file.json');
    curl_setopt_array($ch_create, array(
        CURLOPT_POST           => TRUE,
        CURLOPT_RETURNTRANSFER => TRUE,
        CURLOPT_HTTPHEADER     => array(
            'Content-Type: application/json'
        ),
        CURLOPT_POSTFIELDS     => json_encode($postDataCreate)
    ));

    // Send the request
    $response_create = curl_exec($ch_create);

    // Check for errors
    if ($response_create === FALSE) {
        die(curl_error($ch_create));
    }

    // Decode the response
    $responseDataCreate = json_decode($response_create, TRUE);
    checkResponse($ch_create, $responseDataCreate, 'create_file');
    curl_close($ch_create);

    $file_id = $responseDataCreate['FileId'];

    // DEBUG
    echo 'File created<br>';
    print_r($responseDataCreate);
    echo '<br><br>';
    // END CREATE FILE


    if ($file_id != '') {
        // OPEN FILE UPLOAD
        // The data to send to the API
        $postDataOpen = array(
            'session_id'       => $session_id,            //string (required) - Session ID.
            'file_id'          => $file_id,                //string (required) - File ID.
            'file_size'        => $file_size,            //int (required) - File Size.
            'access_folder_id' => ''                //string - Access folder ID.
        );

        // Setup cURL
        $ch_open = curl_init(API_SERVER . 'v1/upload/open_file_upload.json');
        curl_setopt_array($ch_open, array(
            CURLOPT_POST           => TRUE,
            CURLOPT_RETURNTRANSFER => TRUE,
            CURLOPT_HTTPHEADER     => array(
                'Content-Type: application/json'
            ),
            CURLOPT_POSTFIELDS     => json_encode($postDataOpen)
        ));

        // Send the request
        $response_open = curl_exec($ch_open);

        // Check for errors
        if ($response_open === FALSE) {
            die(curl_error($ch_open));
        }


        // Decode the response
        $responseDataOpen = json_decode($response_open, TRUE);
        checkResponse($ch_open, $response_open, 'open_file_upload');
        curl_close($ch_open);

        $od_temp_location = $responseDataOpen['TempLocation'];

        // DEBUG
        echo 'File opened<br>';
        print_r($responseDataOpen);
        echo '<br><br>';

        // END OPEN FILE UPLOAD


        $cfile = new CURLFile($file_temp_location, 'application/octet-stream');

        $postData = array(
            'session_id'    => $session_id,
            'file_id'       => $file_id,
            'temp_location' => $od_temp_location,
            'chunk_offset'  => 0,
            'chunk_size'    => $file_size,
            'file_data'     => $cfile
        );


// Setup cURL
        $ch = curl_init(API_SERVER . 'v1/upload/upload_file_chunk.json');
        curl_setopt_array($ch, array(
            CURLOPT_POST           => TRUE,
            CURLOPT_RETURNTRANSFER => TRUE,
            CURLOPT_HTTPHEADER     => array(
                'Expect:'
            ),
            CURLOPT_POSTFIELDS     => $postData
        ));

//Send the request
        $response = curl_exec($ch);

        if ($response === FALSE) {
            die(curl_error($ch));
        }

// Decode the response
        $responseData = json_decode($response, TRUE);
        echo "-- Step 3 --\n";

        checkResponse($ch, $responseData, 'upload_file_chunk');
        curl_close($ch);

        echo 'Uploading chunk<br>';
        print_r($responseData);
        echo '<br><br>';


        // CLOSE FILE UPLOAD
        // The data to send to the API
        $postDataClose = array(
            'session_id'       => $session_id,                    //string (required) - Session ID.
            'file_id'          => $file_id,                        //string (required) - File ID.
            'temp_location'    => $od_temp_location,         //string (required) - File temp location.
            'file_size'        => $file_size,                    //int (required) - File size in bytes.
            'file_time'        => time(),                            //int - Time of file creation.
            'access_folder_id' => ''                        //string - Access folder ID.
        );


        // Setup cURL
        $ch_close = curl_init(API_SERVER . 'v1/upload/close_file_upload.json');
        curl_setopt_array($ch_close, array(
            CURLOPT_POST           => TRUE,
            CURLOPT_RETURNTRANSFER => TRUE,
            CURLOPT_HTTPHEADER     => array(
                'Content-Type: application/json'
            ),
            CURLOPT_POSTFIELDS     => json_encode($postDataClose)
        ));

        // Send the request
        $response_close = curl_exec($ch_close);

        // Check for errors
        if ($response_close === FALSE) {
            die(curl_error($ch_close));
        }

        // Decode the response
        $responseDataClose = json_decode($response_close, TRUE);
        checkResponse($ch_close, $responseDataClose, 'close_file_upload');
        curl_close($ch_close);

        // DEBUG
        echo 'File upload closed<br>';
        print_r($responseDataClose);
        echo '<br><br>';

        // END UPLOAD FILE CHUNK
        echo 'File successfully uploaded<br>';

    }
}

?>
<h4>For files, greater than 5Mb, please split file to chunks.</h4>
<form method='POST' enctype='multipart/form-data'>
    Upload file: <input type='file' name='test_file'>
    <input type='submit' value='Upload'>
</form>