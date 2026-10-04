# OVO adapter test data

- `postman_qr_generate_response.json`, `postman_inquiry_response.json`: the
  saved example responses from OVO's official Postman collection
  "[SNAP] Dynamic QRIS V2" (downloaded from
  https://www.ovo.id/partner-integration/dynamic-qris/postman-collection).
  They are copied verbatim, except that the customer name in `userName` has
  been replaced with a placeholder.
- `test_private_key_pkcs8.pem`, `test_private_key_pkcs1.pem`: a throwaway RSA
  key generated for these tests only (the same key in two encodings). It is
  not a credential and is not registered with OVO.
