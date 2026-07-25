<?php
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

$folder_id = '';            //string (required) - Folder ID (0 for root folder).


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
checkResponse($ch, $responseData, 'session_login');
curl_close($ch);
$session_id = $responseData['SessionID'];


?>

<html>
<head>
    <script src='http://code.jquery.com/jquery-latest.min.js'></script>
    <script src='js/resumable.js'></script>
    <script>
        $(document).ready(function () {
            var apiUploadArgs = {
                session_id: '<?php echo $session_id?>',
                folder_id: '<?php echo $folder_id?>'
            };

            var r = new Resumable({
                target: 'https://dev.opendrive.com/api/v1/upload/resumable.json',
                query: function () {
                    return apiUploadArgs
                },
                chunkSize: 1 * 1024 * 1024,
                testChunks: false,
                simultaneousUploads: 1,
                permanentErrors: [400, 401, 402, 403, 404, 409, 413, 415, 500, 503, 507],
                maxChunkRetries: 10
            });


            function add_log(log_message) {
                $('#log-area').append(log_message + '<br>');
            }

            // Resumable.js isn't supported, fall back on a different method
            if (!r.support) {
                alert('Resumable upload not supported.')
            }
            else {
                r.assignDrop($('#dropable-area'));
                r.assignBrowse($('#btn-browse-file'));

                r.on('fileAdded', function (file) {
                    add_log('File added ' + file.file.name);
                    console.log(file);
                });
                r.on('fileSuccess', function (file, message) {
                    add_log('Uploaded ' + file.file.name + ' ' + message);
                    console.log(file);
                    console.log(message);
                });
                r.on('fileError', function (file, message) {
                    delete apiUploadArgs.upload_id;
                    add_log('Error ' + file.file.name + ' ' + message);
                    console.log(file);
                    console.log(message);
                });
                r.on('fileProgress', function(file, data){
                    if(data) {
                        try {
                            data = JSON.parse(data);
                            if (data && data.upload_id) {
                                apiUploadArgs.upload_id = data.upload_id;
                            }
                        } catch (e) {
                        }
                    }
                });
                r.on('progress', function (file) {
                    add_log('Uploaded ' + Math.ceil(r.progress() * 100) + '%');
                    console.log(r.progress());
                });

                r.on('fileRetry', function (file) {
                    add_log('Retrying');
                    console.log('fileRetry');
                });
                r.on('complete', function (file) {
                    delete apiUploadArgs.upload_id;
                    add_log('All upload completed');
                    console.log('complete');
                });

                $('#btn-start').click(function () {
                    r.upload();
                    return false;
                });

                $('#btn-pause').click(function () {
                    r.pause();
                    return false;
                });


            }
        });
    </script>
    <style>
        #dropable-area {
            border: 2px solid grey;
            text-align: center;
            line-height: 300px;
            width: 500px;
            height: 300px;
            font-size: 20px;
        }

        #log-area {
            width: 500px;
            height: 300px;
        }
    </style>
</head>
<body>
<div id='dropable-area'>Drop file here</div>
<br><br>
<input type='file' id='btn-browse-file'>
<input type='button' id='btn-start' value='Start'>
<input type='button' id='btn-pause' value='Pause'>
<br><br>
Upload log:
<div id='log-area'>Ready...<br></div>
</body>
</html>