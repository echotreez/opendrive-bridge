<?php
error_reporting(E_ALL);
ini_set('display_errors', '1');

// config
define('API_SERVER', 'https://dev.opendrive.com/api/');
define('CHUNK_SIZE', 50 * 1024 * 1024);

//path to your file
$file_name = dirname(__FILE__) . '/test_chunk_upload.jpg';
$file_size = filesize($file_name);

//vars
$session_id = ''; // Required Session ID
$folder_id = '0'; // 0 - root folder, otherwise valid folder id
$dest_file_name = 'test_chunk_upload.jpg'; // Destination file name on OpenDrive

function checkResponse($ch, $responseData, $step)
{
    $code = curl_getinfo($ch, CURLINFO_HTTP_CODE);
    if (isset($responseData['error']) || $code != 200) {
        $message = isset($responseData['error']['message']) ? $responseData['error']['message'] : 'Upload error';
        echo "$step error: $code $message";
        exit;
    }
}

function curlPostRequest($ch, $url, $body, $headers = [])
{
    curl_setopt_array($ch, [
        CURLOPT_URL => $url,
        CURLOPT_POST => TRUE,
        CURLOPT_RETURNTRANSFER => TRUE
    ]);

    if (empty($headers)) {
        curl_setopt_array($ch, [
            CURLOPT_POSTFIELDS => json_encode($body),
            CURLOPT_HTTPHEADER => [
                'Content-Type: application/json'
            ]
        ]);
    } else {
        curl_setopt_array($ch, [
            CURLOPT_POSTFIELDS => $body,
            CURLOPT_HTTPHEADER => $headers
        ]);
    }

    $response = curl_exec($ch);

    if ($response === FALSE) {
        die(curl_error($ch));
    }

    return json_decode($response, TRUE);
}

// 1. Create file
$postData = [
    'session_id' => $session_id,
    'folder_id' => $folder_id,
    'file_name' => $dest_file_name,
    'file_size' => $file_size
];

// 1. Setup cURL, create file
$ch = curl_init();

// Decode the response
$responseData = curlPostRequest($ch, API_SERVER . 'v1/upload/create_file.json', $postData);

echo "-- Step 1 --\n";
print_r($responseData);
checkResponse($ch, $responseData, 'create_file');

// 2. Open File for Upload
$file_id = $responseData['FileId'];
$file_time = isset($responseData['DirUpdateTime']) ? $responseData['DirUpdateTime'] : time();

$postData = [
    'session_id' => $session_id,
    'file_id' => $file_id,
    'file_size' => $file_size
];

// Send the request
$responseData = curlPostRequest($ch, API_SERVER . 'v1/upload/open_file_upload.json', $postData);

echo "-- Step 2 --\n";
print_r($responseData);

checkResponse($ch, $responseData, 'open_file_upload');

// 3. Send Chunk 

$temp_location = $responseData['TempLocation'];

echo "-- Step 3 --\n";
$chunk_offset = 0;

$fh = fopen($file_name, 'r');
while(!feof($fh)) {
    $chunk_data = fread($fh, CHUNK_SIZE);
    if($chunk_data === false) {
        echo "Error while reading file";
        exit;
    }

    $chunk_length = strlen($chunk_data);

    $tmp_file = tmpfile();
    fwrite($tmp_file, $chunk_data);
    $tmp_path = stream_get_meta_data($tmp_file)['uri'];

    /*
     * chunk_offset is incremented after each chunk upload based on the length of the uploaded chunk
     * chunk_size is the length of the chunk being uploaded, it will be less than CHUNK_SIZE in case of the last chunk
     *
     * API method expects multipart/form-data
     */
    $postData = [
        'session_id' => $session_id,
        'file_id' => $file_id,
        'temp_location' => $temp_location,
        'chunk_offset' => $chunk_offset,
        'chunk_size' => $chunk_length,
        'file_data' => new CURLFile($tmp_path, 'application/octet-stream', $file_name)
    ];

    echo "Uploading chunk with size $chunk_length at chunk offset $chunk_offset\n";

    // Upload a chunk
    $responseData = curlPostRequest($ch, API_SERVER . 'v1/upload/upload_file_chunk.json', $postData, [
        'Expect:'
    ]);
    fclose($tmp_file);

    print_r($responseData);
    if ($responseData['TotalWritten'] != $chunk_length) {
        echo 'upload_file_chunk step 1 error: chunk incorrectly uploaded';
        exit;
    }

    checkResponse($ch, $responseData, 'upload_file_chunk step1');

    $chunk_offset += $chunk_length;
}

// 5. Close File Upload
$postData = [
    'session_id' => $session_id,
    'file_id' => $file_id,
    'file_size' => $file_size,
    'temp_location' => $temp_location,
    'file_time' => $file_time
];

// Send the request
$responseData = curlPostRequest($ch, API_SERVER . 'v1/upload/close_file_upload.json', $postData);

echo "-- Step 5 --\n";
print_r($responseData);

checkResponse($ch, $responseData, 'close_file_upload');
curl_close($ch);
